package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"relaypanel/internal/domain"
)

type ipv4Range struct {
	start uint32
	end   uint32
}

type storedGeoRange struct {
	start    uint32
	end      uint32
	country  string
	province string
	city     string
	isp      string
}

func ipv4Number(value string) (uint32, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || !addr.Is4() {
		return 0, false
	}
	bytes := addr.As4()
	return uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3]), true
}

func ipv4String(value uint32) string {
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}).String()
}

func supportedGeoCountry(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || value == "中国" || strings.EqualFold(value, "china") || strings.EqualFold(value, "cn")
}

func (s *Store) invalidateAccessCache() {
	s.accessMu.Lock()
	s.accessCache = map[string]domain.AccessPolicy{}
	s.accessMu.Unlock()
}

func (s *Store) resolveAccessPolicy(ctx context.Context, rule domain.ForwardRule) (domain.AccessPolicy, error) {
	policy := rule.AccessPolicy
	if !policy.Enabled || (len(policy.AllowRegions) == 0 && len(policy.DenyRegions) == 0) {
		return policy, nil
	}
	key := fmt.Sprintf("%s:%d", rule.ID, rule.Revision)
	s.accessMu.RLock()
	cached, ok := s.accessCache[key]
	s.accessMu.RUnlock()
	if ok {
		return cached, nil
	}
	allow, err := s.expandRegions(ctx, policy.AllowRegions)
	if err != nil {
		return policy, err
	}
	deny, err := s.expandRegions(ctx, policy.DenyRegions)
	if err != nil {
		return policy, err
	}
	policy.ResolvedAllowRanges = allow
	policy.ResolvedDenyRanges = deny
	s.accessMu.Lock()
	s.accessCache[key] = policy
	s.accessMu.Unlock()
	return policy, nil
}

func splitRegion(value string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(value), "/", 2)
	if len(parts) == 0 {
		return "", ""
	}
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func (s *Store) expandRegions(ctx context.Context, regions []string) ([]string, error) {
	var values []ipv4Range
	seen := map[string]bool{}
	for _, selection := range regions {
		selection = strings.TrimSpace(selection)
		if selection == "" || seen[selection] {
			continue
		}
		seen[selection] = true
		province, city := splitRegion(selection)
		query := `SELECT start_ip,end_ip FROM geo_ip_ranges WHERE country='中国' AND province=?`
		args := []any{province}
		if city != "" {
			query += ` AND city=?`
			args = append(args, city)
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var start, end int64
			if err := rows.Scan(&start, &end); err != nil {
				_ = rows.Close()
				return nil, err
			}
			values = append(values, ipv4Range{start: uint32(start), end: uint32(end)})
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].start == values[j].start {
			return values[i].end < values[j].end
		}
		return values[i].start < values[j].start
	})
	merged := values[:1]
	for _, current := range values[1:] {
		last := &merged[len(merged)-1]
		if current.start <= last.end || (last.end != ^uint32(0) && current.start == last.end+1) {
			if current.end > last.end {
				last.end = current.end
			}
			continue
		}
		merged = append(merged, current)
	}
	out := make([]string, 0, len(merged))
	for _, item := range merged {
		if item.start == item.end {
			out = append(out, ipv4String(item.start))
		} else {
			out = append(out, ipv4String(item.start)+"-"+ipv4String(item.end))
		}
	}
	return out, nil
}

func (s *Store) ReplaceGeoRanges(ctx context.Context, ranges []domain.GeoRange) error {
	return s.replaceGeoSource(ctx, "geo_ip2region_ranges", "geo_updated_at", ranges)
}

func (s *Store) ReplaceMaxMindRanges(ctx context.Context, ranges []domain.GeoRange) error {
	return s.replaceGeoSource(ctx, "geo_maxmind_ranges", "maxmind_updated_at", ranges)
}

func (s *Store) replaceGeoSource(ctx context.Context, table, updatedKey string, ranges []domain.GeoRange) error {
	if len(ranges) == 0 {
		return errors.New("IP 地区库没有可导入的数据")
	}
	if table != "geo_ip2region_ranges" && table != "geo_maxmind_ranges" {
		return errors.New("未知的 IP 地区库来源")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO `+table+`(start_ip,end_ip,country,province,city,isp) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, item := range ranges {
		start, okStart := ipv4Number(item.StartIP)
		end, okEnd := ipv4Number(item.EndIP)
		if !okStart || !okEnd || start > end || !supportedGeoCountry(item.Country) {
			continue
		}
		if table == "geo_maxmind_ranges" && strings.TrimSpace(item.Province) == "" {
			continue
		}
		if _, err = stmt.ExecContext(ctx, int64(start), int64(end), "中国", strings.TrimSpace(item.Province), strings.TrimSpace(item.City), strings.TrimSpace(item.ISP)); err != nil {
			return err
		}
	}
	if err = s.rebuildEffectiveGeoRanges(ctx, tx); err != nil {
		return err
	}
	updatedAt := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, updatedKey, updatedAt.Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err = s.bumpRevision(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.invalidateAccessCache()
	return nil
}

func readStoredGeoRanges(ctx context.Context, tx *sql.Tx, table string) ([]storedGeoRange, error) {
	rows, err := tx.QueryContext(ctx, `SELECT start_ip,end_ip,country,province,city,isp FROM `+table+` WHERE country='中国' ORDER BY start_ip,end_ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []storedGeoRange
	for rows.Next() {
		var start, end int64
		var item storedGeoRange
		if err := rows.Scan(&start, &end, &item.country, &item.province, &item.city, &item.isp); err != nil {
			return nil, err
		}
		item.start, item.end = uint32(start), uint32(end)
		values = append(values, item)
	}
	return values, rows.Err()
}

// rebuildEffectiveGeoRanges gives an existing ip2region province precedence
// and uses MaxMind only for uncovered or province-less address space.
func (s *Store) rebuildEffectiveGeoRanges(ctx context.Context, tx *sql.Tx) error {
	base, err := readStoredGeoRanges(ctx, tx, "geo_ip2region_ranges")
	if err != nil {
		return err
	}
	maxmind, err := readStoredGeoRanges(ctx, tx, "geo_maxmind_ranges")
	if err != nil {
		return err
	}
	known := make([]storedGeoRange, 0, len(base))
	for _, item := range base {
		if strings.TrimSpace(item.province) != "" {
			known = append(known, item)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM geo_ip_ranges`); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO geo_ip_ranges(start_ip,end_ip,country,province,city,isp) VALUES(?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	insert := func(item storedGeoRange) error {
		_, err := stmt.ExecContext(ctx, int64(item.start), int64(item.end), "中国", item.province, item.city, item.isp)
		return err
	}
	for _, item := range known {
		if err := insert(item); err != nil {
			return err
		}
	}
	knownIndex := 0
	for _, supplement := range maxmind {
		if supplement.province == "" {
			continue
		}
		cursor := uint64(supplement.start)
		end := uint64(supplement.end)
		for knownIndex < len(known) && uint64(known[knownIndex].end) < cursor {
			knownIndex++
		}
		for index := knownIndex; index < len(known) && uint64(known[index].start) <= end; index++ {
			blocked := known[index]
			if uint64(blocked.start) > cursor {
				gap := supplement
				gap.start = uint32(cursor)
				gap.end = uint32(min(end, uint64(blocked.start)-1))
				if err := insert(gap); err != nil {
					return err
				}
			}
			if uint64(blocked.end) >= end {
				cursor = end + 1
				break
			}
			if uint64(blocked.end)+1 > cursor {
				cursor = uint64(blocked.end) + 1
			}
		}
		if cursor <= end {
			gap := supplement
			gap.start, gap.end = uint32(cursor), uint32(end)
			if err := insert(gap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) MaxMindRangeCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM geo_maxmind_ranges WHERE country='中国'`).Scan(&count)
	return count, err
}

func (s *Store) GeoStatus(ctx context.Context) (domain.GeoStatus, error) {
	var status domain.GeoStatus
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM geo_ip_ranges WHERE country='中国'`).Scan(&status.Ranges); err != nil {
		return status, err
	}
	status.Ready = status.Ranges > 0
	if value, err := s.GetSetting(ctx, "geo_updated_at"); err == nil {
		status.UpdatedAt, _ = time.Parse(time.RFC3339, value)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT province,city FROM geo_ip_ranges WHERE country='中国' AND province<>'' GROUP BY province,city ORDER BY province,city`)
	if err != nil {
		return status, err
	}
	defer rows.Close()
	byProvince := map[string][]string{}
	var order []string
	for rows.Next() {
		var province, city string
		if err := rows.Scan(&province, &city); err != nil {
			return status, err
		}
		if _, ok := byProvince[province]; !ok {
			order = append(order, province)
			byProvince[province] = []string{}
		}
		if city != "" {
			byProvince[province] = append(byProvince[province], city)
		}
	}
	for _, province := range order {
		status.Regions = append(status.Regions, domain.GeoRegion{Province: province, Cities: byProvince[province]})
	}
	return status, rows.Err()
}

func ownsConnectionReport(deployment domain.Deployment) bool {
	return deployment.Role == domain.NodeRoleIngress || deployment.Role == domain.NodeRoleBoth || (deployment.Rule.Mode == domain.ForwardModeExitOnly && deployment.Role == domain.NodeRoleEgress)
}

func (s *Store) UpsertConnectionReport(ctx context.Context, nodeID string, report domain.ConnectionReport) error {
	deployments, err := s.DeploymentsForNode(ctx, nodeID)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, deployment := range deployments {
		if ownsConnectionReport(deployment) {
			allowed[deployment.Rule.ID] = true
		}
	}
	capturedAt := report.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO connection_status(node_id,available,error,captured_at,total_tcp_connections,total_udp_sessions) VALUES(?,?,?,?,?,?) ON CONFLICT(node_id) DO UPDATE SET available=excluded.available,error=excluded.error,captured_at=excluded.captured_at,total_tcp_connections=excluded.total_tcp_connections,total_udp_sessions=excluded.total_udp_sessions`, nodeID, boolInt(report.Available), report.Error, capturedAt.Unix(), report.TotalTCPConnections, report.TotalUDPSessions); err != nil {
		return err
	}
	if report.Available {
		for _, ruleID := range report.RuleIDs {
			if allowed[ruleID] {
				if _, err = tx.ExecContext(ctx, `UPDATE connection_sources SET tcp_connections=0,udp_sessions=0,captured_at=? WHERE rule_id=? AND node_id=?`, capturedAt.Unix(), ruleID, nodeID); err != nil {
					return err
				}
			}
		}
		if len(report.Entries) > 5000 {
			report.Entries = report.Entries[:5000]
		}
		for _, entry := range report.Entries {
			if !allowed[entry.RuleID] || entry.TCPConnections < 0 || entry.UDPSessions < 0 {
				continue
			}
			sourceNumber, ok := ipv4Number(entry.SourceIP)
			if !ok {
				continue
			}
			lastSeen := capturedAt
			if entry.TCPConnections == 0 && entry.UDPSessions == 0 {
				lastSeen = time.Time{}
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO connection_sources(rule_id,node_id,source_ip,source_ip_number,tcp_connections,udp_sessions,captured_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(rule_id,node_id,source_ip) DO UPDATE SET source_ip_number=excluded.source_ip_number,tcp_connections=excluded.tcp_connections,udp_sessions=excluded.udp_sessions,captured_at=excluded.captured_at,last_seen_at=CASE WHEN excluded.last_seen_at>0 THEN excluded.last_seen_at ELSE connection_sources.last_seen_at END`, entry.RuleID, nodeID, entry.SourceIP, int64(sourceNumber), entry.TCPConnections, entry.UDPSessions, capturedAt.Unix(), unix(lastSeen))
			if err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM connection_sources WHERE last_seen_at>0 AND last_seen_at<?`, time.Now().UTC().Add(-7*24*time.Hour).Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListConnections(ctx context.Context) (domain.ConnectionsResponse, error) {
	var response domain.ConnectionsResponse
	statusRows, err := s.db.QueryContext(ctx, `SELECT node_id,available,error,captured_at,total_tcp_connections,total_udp_sessions FROM connection_status ORDER BY node_id`)
	if err != nil {
		return response, err
	}
	for statusRows.Next() {
		var item domain.ConnectionStatus
		var available int
		var captured int64
		if err := statusRows.Scan(&item.NodeID, &available, &item.Error, &captured, &item.TotalTCPConnections, &item.TotalUDPSessions); err != nil {
			_ = statusRows.Close()
			return response, err
		}
		item.Available = available == 1
		item.CapturedAt = fromUnix(captured)
		response.Statuses = append(response.Statuses, item)
	}
	_ = statusRows.Close()
	rows, err := s.db.QueryContext(ctx, `
		SELECT cs.rule_id,cs.node_id,cs.source_ip,cs.tcp_connections,cs.udp_sessions,
		       cs.captured_at,cs.last_seen_at,
		       COALESCE(geo.country,''),COALESCE(geo.province,''),COALESCE(geo.city,''),
		       COALESCE(NULLIF(geo.isp,''),base.isp,'')
		FROM connection_sources cs
		LEFT JOIN geo_ip_ranges geo ON geo.rowid=(
			SELECT candidate.rowid FROM geo_ip_ranges candidate
			WHERE candidate.start_ip<=cs.source_ip_number AND candidate.end_ip>=cs.source_ip_number
			  AND candidate.country='中国'
			ORDER BY candidate.start_ip DESC LIMIT 1
		)
		LEFT JOIN geo_ip2region_ranges base ON base.rowid=(
			SELECT candidate.rowid FROM geo_ip2region_ranges candidate
			WHERE candidate.start_ip<=cs.source_ip_number AND candidate.end_ip>=cs.source_ip_number
			  AND candidate.country='中国'
			ORDER BY candidate.start_ip DESC LIMIT 1
		)
		WHERE cs.last_seen_at>=?
		ORDER BY cs.tcp_connections+cs.udp_sessions DESC,cs.last_seen_at DESC LIMIT 2000`, time.Now().UTC().Add(-7*24*time.Hour).Unix())
	if err != nil {
		return response, err
	}
	staleBefore := time.Now().UTC().Add(-45 * time.Second)
	for rows.Next() {
		var item domain.ConnectionSource
		var captured, lastSeen int64
		if err := rows.Scan(&item.RuleID, &item.NodeID, &item.SourceIP, &item.TCPConnections, &item.UDPSessions, &captured, &lastSeen, &item.Country, &item.Province, &item.City, &item.ISP); err != nil {
			return response, err
		}
		item.CapturedAt = fromUnix(captured)
		item.LastSeenAt = fromUnix(lastSeen)
		if item.CapturedAt.Before(staleBefore) {
			item.TCPConnections = 0
			item.UDPSessions = 0
		}
		response.Sources = append(response.Sources, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return response, err
	}
	if err := rows.Close(); err != nil {
		return response, err
	}
	return response, nil
}
