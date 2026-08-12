package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"relaypanel/internal/domain"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, role TEXT NOT NULL,
			public_address TEXT NOT NULL DEFAULT '', private_address TEXT NOT NULL DEFAULT '',
			public_interface TEXT NOT NULL DEFAULT '', private_interface TEXT NOT NULL DEFAULT '',
			agent_token_hash TEXT NOT NULL, agent_version TEXT NOT NULL DEFAULT '',
			applied_revision INTEGER NOT NULL DEFAULT 0, apply_status TEXT NOT NULL DEFAULT 'pending',
			apply_error TEXT NOT NULL DEFAULT '', last_seen_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forward_rules (
			id TEXT PRIMARY KEY, mode TEXT NOT NULL DEFAULT 'dual_managed', name TEXT NOT NULL, protocol TEXT NOT NULL,
			ingress_node_id TEXT NOT NULL REFERENCES nodes(id),
			egress_node_id TEXT NOT NULL REFERENCES nodes(id),
			listen_address TEXT NOT NULL DEFAULT '0.0.0.0', listen_port INTEGER NOT NULL,
			relay_port INTEGER NOT NULL, target_host TEXT NOT NULL, target_port INTEGER NOT NULL,
			engine TEXT NOT NULL DEFAULT 'nftables', upload_mbps INTEGER NOT NULL DEFAULT 0,
			download_mbps INTEGER NOT NULL DEFAULT 0, burst_kbytes INTEGER NOT NULL DEFAULT 512,
			enabled INTEGER NOT NULL DEFAULT 1, revision INTEGER NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			UNIQUE(ingress_node_id, listen_port, protocol),
			UNIQUE(egress_node_id, relay_port, protocol)
		)`,
		`CREATE TABLE IF NOT EXISTS traffic_minute (
			rule_id TEXT NOT NULL REFERENCES forward_rules(id) ON DELETE CASCADE,
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			bucket INTEGER NOT NULL, upload_bytes INTEGER NOT NULL DEFAULT 0,
			download_bytes INTEGER NOT NULL DEFAULT 0, upload_packets INTEGER NOT NULL DEFAULT 0,
			download_packets INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(rule_id, node_id, bucket)
		)`,
		`CREATE TABLE IF NOT EXISTS traffic_baselines (
			rule_id TEXT NOT NULL REFERENCES forward_rules(id) ON DELETE CASCADE,
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			upload_bytes INTEGER NOT NULL DEFAULT 0, download_bytes INTEGER NOT NULL DEFAULT 0,
			upload_packets INTEGER NOT NULL DEFAULT 0, download_packets INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(rule_id, node_id)
		)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT, action TEXT NOT NULL,
			resource_type TEXT NOT NULL, resource_id TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_last_seen_at ON nodes(last_seen_at)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_nodes ON forward_rules(ingress_node_id, egress_node_id)`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_bucket ON traffic_minute(bucket)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "forward_rules", "mode", `ALTER TABLE forward_rules ADD COLUMN mode TEXT NOT NULL DEFAULT 'dual_managed'`); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings(key, value) VALUES ('revision', '1')`)
	_, _ = s.db.ExecContext(ctx, `PRAGMA optimize`)
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, alter string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return value, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) Revision(ctx context.Context) (int64, error) {
	var revision int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM settings WHERE key='revision'`).Scan(&revision)
	return revision, err
}

func (s *Store) bumpRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE settings SET value=CAST(value AS INTEGER)+1 WHERE key='revision'`); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM settings WHERE key='revision'`).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func (s *Store) CreateNode(ctx context.Context, n domain.Node, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id,name,role,public_address,private_address,public_interface,private_interface,agent_token_hash,created_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		n.ID, n.Name, n.Role, n.PublicAddress, n.PrivateAddress, n.PublicInterface, n.PrivateInterface, tokenHash, unix(n.CreatedAt))
	if err == nil {
		s.audit(ctx, "create", "node", n.ID, n.Name)
	}
	return err
}

func scanNode(scanner interface{ Scan(...any) error }) (domain.Node, string, error) {
	var n domain.Node
	var tokenHash, applyStatus string
	var lastSeen, created int64
	err := scanner.Scan(&n.ID, &n.Name, &n.Role, &n.PublicAddress, &n.PrivateAddress, &n.PublicInterface, &n.PrivateInterface, &tokenHash, &n.AgentVersion, &n.AppliedRevision, &applyStatus, &lastSeen, &created)
	n.LastSeenAt = fromUnix(lastSeen)
	n.CreatedAt = fromUnix(created)
	if time.Since(n.LastSeenAt) <= 45*time.Second {
		n.Status = "online"
	} else {
		n.Status = "offline"
	}
	return n, tokenHash, err
}

const nodeColumns = `id,name,role,public_address,private_address,public_interface,private_interface,agent_token_hash,agent_version,applied_revision,apply_status,last_seen_at,created_at`

func (s *Store) ListNodes(ctx context.Context) ([]domain.Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []domain.Node
	for rows.Next() {
		n, _, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

func (s *Store) GetNode(ctx context.Context, id string) (domain.Node, string, error) {
	n, hash, err := scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return n, hash, err
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	s.audit(ctx, "delete", "node", id, "")
	return nil
}

func (s *Store) UpdateHeartbeat(ctx context.Context, id, version, status, applyErr string, revision int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET agent_version=?,applied_revision=?,apply_status=?,apply_error=?,last_seen_at=? WHERE id=?`, version, revision, status, applyErr, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanRule(scanner interface{ Scan(...any) error }) (domain.ForwardRule, error) {
	var r domain.ForwardRule
	var enabled int
	var created, updated int64
	err := scanner.Scan(&r.ID, &r.Mode, &r.Name, &r.Protocol, &r.IngressNodeID, &r.EgressNodeID, &r.ListenAddress, &r.ListenPort, &r.RelayPort, &r.TargetHost, &r.TargetPort, &r.Engine, &r.UploadMbps, &r.DownloadMbps, &r.BurstKBytes, &enabled, &r.Revision, &created, &updated)
	r.Enabled = enabled == 1
	r.CreatedAt = fromUnix(created)
	r.UpdatedAt = fromUnix(updated)
	return r, err
}

const ruleColumns = `id,mode,name,protocol,ingress_node_id,egress_node_id,listen_address,listen_port,relay_port,target_host,target_port,engine,upload_mbps,download_mbps,burst_kbytes,enabled,revision,created_at,updated_at`

func (s *Store) ListRules(ctx context.Context) ([]domain.ForwardRule, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleColumns+` FROM forward_rules ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []domain.ForwardRule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (s *Store) GetRule(ctx context.Context, id string) (domain.ForwardRule, error) {
	r, err := scanRule(s.db.QueryRowContext(ctx, `SELECT `+ruleColumns+` FROM forward_rules WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return r, err
}

func (s *Store) SaveRule(ctx context.Context, r domain.ForwardRule) (domain.ForwardRule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	revision, err := s.bumpRevision(ctx, tx)
	if err != nil {
		return r, err
	}
	now := time.Now().UTC()
	r.Revision = revision
	r.UpdatedAt = now
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO forward_rules(`+ruleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET mode=excluded.mode,name=excluded.name,protocol=excluded.protocol,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,listen_port=excluded.listen_port,relay_port=excluded.relay_port,target_host=excluded.target_host,target_port=excluded.target_port,engine=excluded.engine,upload_mbps=excluded.upload_mbps,download_mbps=excluded.download_mbps,burst_kbytes=excluded.burst_kbytes,enabled=excluded.enabled,revision=excluded.revision,updated_at=excluded.updated_at`, r.ID, r.Mode, r.Name, r.Protocol, r.IngressNodeID, r.EgressNodeID, r.ListenAddress, r.ListenPort, r.RelayPort, r.TargetHost, r.TargetPort, r.Engine, r.UploadMbps, r.DownloadMbps, r.BurstKBytes, boolInt(r.Enabled), r.Revision, unix(r.CreatedAt), unix(r.UpdatedAt))
	if err != nil {
		return r, err
	}
	if err = tx.Commit(); err != nil {
		return r, err
	}
	s.audit(ctx, "save", "rule", r.ID, r.Name)
	return r, nil
}

func (s *Store) DeleteRule(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM forward_rules WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if _, err = s.bumpRevision(ctx, tx); err != nil {
		return err
	}
	if err = tx.Commit(); err == nil {
		s.audit(ctx, "delete", "rule", id, "")
	}
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) DeploymentsForNode(ctx context.Context, nodeID string) ([]domain.Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ruleColumns+` FROM forward_rules WHERE enabled=1 AND (ingress_node_id=? OR egress_node_id=?) ORDER BY id`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Deployment
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		role := domain.NodeRoleEgress
		if r.Mode != domain.ForwardModeExitOnly && r.IngressNodeID == nodeID {
			role = domain.NodeRoleIngress
		}
		if r.Mode != domain.ForwardModeExitOnly && r.IngressNodeID == nodeID && r.EgressNodeID == nodeID {
			role = domain.NodeRoleBoth
		}
		out = append(out, domain.Deployment{Rule: r, Role: role})
	}
	return out, rows.Err()
}

func (s *Store) AddTraffic(ctx context.Context, nodeID string, deltas []domain.TrafficDelta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range deltas {
		if d.Cumulative {
			rawUpBytes, rawDownBytes, rawUpPackets, rawDownPackets := d.UploadBytes, d.DownloadBytes, d.UploadPackets, d.DownloadPackets
			var oldUpBytes, oldDownBytes, oldUpPackets, oldDownPackets int64
			err = tx.QueryRowContext(ctx, `SELECT upload_bytes,download_bytes,upload_packets,download_packets FROM traffic_baselines WHERE rule_id=? AND node_id=?`, d.RuleID, nodeID).Scan(&oldUpBytes, &oldDownBytes, &oldUpPackets, &oldDownPackets)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			d.UploadBytes = counterDelta(d.UploadBytes, oldUpBytes)
			d.DownloadBytes = counterDelta(d.DownloadBytes, oldDownBytes)
			d.UploadPackets = counterDelta(d.UploadPackets, oldUpPackets)
			d.DownloadPackets = counterDelta(d.DownloadPackets, oldDownPackets)
			_, err = tx.ExecContext(ctx, `INSERT INTO traffic_baselines(rule_id,node_id,upload_bytes,download_bytes,upload_packets,download_packets) VALUES(?,?,?,?,?,?) ON CONFLICT(rule_id,node_id) DO UPDATE SET upload_bytes=excluded.upload_bytes,download_bytes=excluded.download_bytes,upload_packets=excluded.upload_packets,download_packets=excluded.download_packets`, d.RuleID, nodeID, rawUpBytes, rawDownBytes, rawUpPackets, rawDownPackets)
			if err != nil {
				return err
			}
		}
		bucket := d.CapturedAt.UTC().Truncate(time.Minute).Unix()
		if bucket == 0 {
			bucket = time.Now().UTC().Truncate(time.Minute).Unix()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO traffic_minute(rule_id,node_id,bucket,upload_bytes,download_bytes,upload_packets,download_packets) VALUES(?,?,?,?,?,?,?) ON CONFLICT(rule_id,node_id,bucket) DO UPDATE SET upload_bytes=upload_bytes+excluded.upload_bytes,download_bytes=download_bytes+excluded.download_bytes,upload_packets=upload_packets+excluded.upload_packets,download_packets=download_packets+excluded.download_packets`, d.RuleID, nodeID, bucket, d.UploadBytes, d.DownloadBytes, d.UploadPackets, d.DownloadPackets)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func counterDelta(current, previous int64) int64 {
	if current >= previous {
		return current - previous
	}
	return current
}

func (s *Store) Traffic(ctx context.Context, ruleID string, since time.Time) ([]domain.TrafficPoint, error) {
	query := `SELECT bucket,SUM(upload_bytes),SUM(download_bytes),SUM(upload_packets),SUM(download_packets) FROM traffic_minute WHERE bucket>=?`
	args := []any{since.Unix()}
	if ruleID != "" {
		query += ` AND rule_id=?`
		args = append(args, ruleID)
	}
	query += ` GROUP BY bucket ORDER BY bucket`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.TrafficPoint
	for rows.Next() {
		var p domain.TrafficPoint
		var b int64
		if err := rows.Scan(&b, &p.UploadBytes, &p.DownloadBytes, &p.UploadPackets, &p.DownloadPackets); err != nil {
			return nil, err
		}
		p.Bucket = fromUnix(b)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Summary(ctx context.Context) (domain.DashboardSummary, error) {
	var d domain.DashboardSummary
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),SUM(CASE WHEN last_seen_at>=? THEN 1 ELSE 0 END) FROM nodes`, time.Now().Add(-45*time.Second).Unix()).Scan(&d.TotalNodes, &d.OnlineNodes); err != nil {
		return d, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(enabled),0) FROM forward_rules`).Scan(&d.TotalRules, &d.EnabledRules); err != nil {
		return d, err
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(upload_bytes),0),COALESCE(SUM(download_bytes),0) FROM traffic_minute WHERE bucket>=?`, today.Unix()).Scan(&d.TodayUpload, &d.TodayDownload)
	points, err := s.Traffic(ctx, "", time.Now().UTC().Add(-24*time.Hour))
	d.RecentTraffic = points
	return d, err
}

func (s *Store) audit(ctx context.Context, action, kind, id, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_logs(action,resource_type,resource_id,detail,created_at) VALUES(?,?,?,?,?)`, action, kind, id, detail, time.Now().Unix())
}
