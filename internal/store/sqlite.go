package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"relaypanel/internal/domain"
)

var ErrNotFound = errors.New("not found")

var trafficLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type Store struct {
	db               *sql.DB
	lastTrafficPrune atomic.Int64
}

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
		`CREATE TABLE IF NOT EXISTS lines (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, mode TEXT NOT NULL DEFAULT 'dual_managed',
			ingress_node_id TEXT NOT NULL REFERENCES nodes(id),
			egress_node_id TEXT NOT NULL REFERENCES nodes(id),
			listen_address TEXT NOT NULL DEFAULT '', relay_port_range TEXT NOT NULL DEFAULT '',
			engine TEXT NOT NULL DEFAULT 'nftables',
			enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS forward_rules (
			id TEXT PRIMARY KEY, line_id TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT 'dual_managed', name TEXT NOT NULL, protocol TEXT NOT NULL,
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
		`CREATE TABLE IF NOT EXISTS traffic_daily (
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
		`CREATE INDEX IF NOT EXISTS idx_lines_nodes ON lines(ingress_node_id, egress_node_id)`,
		`CREATE INDEX IF NOT EXISTS idx_rules_line ON forward_rules(line_id)`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_bucket ON traffic_minute(bucket)`,
		`CREATE INDEX IF NOT EXISTS idx_traffic_daily_bucket ON traffic_daily(bucket)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := s.ensureColumn(ctx, "forward_rules", "mode", `ALTER TABLE forward_rules ADD COLUMN mode TEXT NOT NULL DEFAULT 'dual_managed'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "forward_rules", "line_id", `ALTER TABLE forward_rules ADD COLUMN line_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "relay_port_range", `ALTER TABLE lines ADD COLUMN relay_port_range TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO settings(key, value) VALUES ('revision', '1')`)
	var dailyTimezone string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='traffic_daily_timezone'`).Scan(&dailyTimezone)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read daily traffic timezone: %w", err)
	}
	if dailyTimezone != "Asia/Shanghai" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// Existing releases stored one aggregate per UTC day. Shift those buckets
		// to Beijing midnight so upgrades preserve long-term totals. The minute
		// detail backfill below covers databases that did not have daily rows yet.
		if _, err = tx.ExecContext(ctx, `UPDATE traffic_daily SET bucket=bucket-28800`); err != nil {
			return fmt.Errorf("shift daily traffic to Beijing time: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO traffic_daily(rule_id,node_id,bucket,upload_bytes,download_bytes,upload_packets,download_packets)
			SELECT rule_id,node_id,
				CAST(strftime('%s',bucket,'unixepoch','+8 hours','start of day') AS INTEGER)-28800,
				SUM(upload_bytes),SUM(download_bytes),SUM(upload_packets),SUM(download_packets)
			FROM traffic_minute
			GROUP BY rule_id,node_id,CAST(strftime('%s',bucket,'unixepoch','+8 hours','start of day') AS INTEGER)-28800
			ON CONFLICT(rule_id,node_id,bucket) DO NOTHING`); err != nil {
			return fmt.Errorf("backfill Beijing daily traffic: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES ('traffic_daily_timezone','Asia/Shanghai') ON CONFLICT(key) DO UPDATE SET value=excluded.value`); err != nil {
			return fmt.Errorf("save daily traffic timezone: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
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
	var tokenHash string
	var lastSeen, created int64
	err := scanner.Scan(&n.ID, &n.Name, &n.Role, &n.PublicAddress, &n.PrivateAddress, &n.PublicInterface, &n.PrivateInterface, &tokenHash, &n.AgentVersion, &n.AppliedRevision, &n.ApplyStatus, &n.ApplyError, &lastSeen, &created)
	n.LastSeenAt = fromUnix(lastSeen)
	n.CreatedAt = fromUnix(created)
	if time.Since(n.LastSeenAt) <= 45*time.Second {
		n.Status = "online"
	} else {
		n.Status = "offline"
	}
	return n, tokenHash, err
}

const nodeColumns = `id,name,role,public_address,private_address,public_interface,private_interface,agent_token_hash,agent_version,applied_revision,apply_status,apply_error,last_seen_at,created_at`

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

func (s *Store) UpdateNode(ctx context.Context, n domain.Node) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET name=?,role=?,public_address=?,private_address=?,public_interface=?,private_interface=? WHERE id=?`, n.Name, n.Role, n.PublicAddress, n.PrivateAddress, n.PublicInterface, n.PrivateInterface, n.ID)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	s.audit(ctx, "update", "node", n.ID, n.Name)
	return nil
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

func (s *Store) UpdateHeartbeat(ctx context.Context, id, version, status, applyErr string, revision int64, network domain.NetworkInfo) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	node, _, err := scanNode(tx.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if node.PublicAddress == "" {
		node.PublicAddress = network.PublicAddress
		if network.PublicInterface != "" && (node.PublicInterface == "" || node.PublicInterface == "eth0") {
			node.PublicInterface = network.PublicInterface
		}
	}
	if node.PrivateAddress == "" {
		node.PrivateAddress = network.PrivateAddress
		if network.PrivateInterface != "" && (node.PrivateInterface == "" || node.PrivateInterface == "wg0") {
			node.PrivateInterface = network.PrivateInterface
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE nodes SET public_address=?,private_address=?,public_interface=?,private_interface=?,agent_version=?,applied_revision=?,apply_status=?,apply_error=?,last_seen_at=? WHERE id=?`, node.PublicAddress, node.PrivateAddress, node.PublicInterface, node.PrivateInterface, version, revision, status, applyErr, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

func scanLine(scanner interface{ Scan(...any) error }) (domain.Line, error) {
	var line domain.Line
	var enabled int
	var created, updated int64
	err := scanner.Scan(&line.ID, &line.Name, &line.Mode, &line.IngressNodeID, &line.EgressNodeID, &line.ListenAddress, &line.RelayPortRange, &line.Engine, &enabled, &created, &updated)
	line.Enabled = enabled == 1
	line.CreatedAt = fromUnix(created)
	line.UpdatedAt = fromUnix(updated)
	return line, err
}

const lineColumns = `id,name,mode,ingress_node_id,egress_node_id,listen_address,relay_port_range,engine,enabled,created_at,updated_at`

func (s *Store) ListLines(ctx context.Context) ([]domain.Line, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lineColumns+` FROM lines ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []domain.Line
	for rows.Next() {
		line, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

func (s *Store) GetLine(ctx context.Context, id string) (domain.Line, error) {
	line, err := scanLine(s.db.QueryRowContext(ctx, `SELECT `+lineColumns+` FROM lines WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return line, err
}

func (s *Store) SaveLine(ctx context.Context, line domain.Line) (domain.Line, error) {
	now := time.Now().UTC()
	line.UpdatedAt = now
	if line.CreatedAt.IsZero() {
		line.CreatedAt = now
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO lines(`+lineColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,mode=excluded.mode,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,relay_port_range=excluded.relay_port_range,engine=excluded.engine,enabled=excluded.enabled,updated_at=excluded.updated_at`, line.ID, line.Name, line.Mode, line.IngressNodeID, line.EgressNodeID, line.ListenAddress, line.RelayPortRange, line.Engine, boolInt(line.Enabled), unix(line.CreatedAt), unix(line.UpdatedAt))
	if err == nil {
		s.audit(ctx, "save", "line", line.ID, line.Name)
	}
	return line, err
}

// SaveLineRules changes a line and every rule that belongs to it in one
// transaction. This prevents Agents from observing a line whose rules still
// reference the previous ingress or egress server.
func (s *Store) SaveLineRules(ctx context.Context, line domain.Line, rules []domain.ForwardRule) (domain.Line, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return line, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	line.UpdatedAt = now
	if line.CreatedAt.IsZero() {
		line.CreatedAt = now
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO lines(`+lineColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,mode=excluded.mode,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,relay_port_range=excluded.relay_port_range,engine=excluded.engine,enabled=excluded.enabled,updated_at=excluded.updated_at`, line.ID, line.Name, line.Mode, line.IngressNodeID, line.EgressNodeID, line.ListenAddress, line.RelayPortRange, line.Engine, boolInt(line.Enabled), unix(line.CreatedAt), unix(line.UpdatedAt))
	if err != nil {
		return line, err
	}
	if len(rules) > 0 {
		revision, err := s.bumpRevision(ctx, tx)
		if err != nil {
			return line, err
		}
		for i := range rules {
			rules[i].Revision = revision
			rules[i].UpdatedAt = now
			_, err = tx.ExecContext(ctx, `UPDATE forward_rules SET mode=?,ingress_node_id=?,egress_node_id=?,listen_address=?,relay_port=?,engine=?,revision=?,updated_at=? WHERE id=? AND line_id=?`, rules[i].Mode, rules[i].IngressNodeID, rules[i].EgressNodeID, rules[i].ListenAddress, rules[i].RelayPort, rules[i].Engine, revision, unix(now), rules[i].ID, line.ID)
			if err != nil {
				return line, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return line, err
	}
	s.audit(ctx, "save", "line", line.ID, line.Name)
	return line, nil
}

func (s *Store) DeleteLine(ctx context.Context, id string) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM forward_rules WHERE line_id=?`, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return errors.New("line still has rules")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM lines WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	s.audit(ctx, "delete", "line", id, "")
	return nil
}

func scanRule(scanner interface{ Scan(...any) error }) (domain.ForwardRule, error) {
	var r domain.ForwardRule
	var enabled int
	var created, updated int64
	err := scanner.Scan(&r.ID, &r.LineID, &r.Mode, &r.Name, &r.Protocol, &r.IngressNodeID, &r.EgressNodeID, &r.ListenAddress, &r.ListenPort, &r.RelayPort, &r.TargetHost, &r.TargetPort, &r.Engine, &r.UploadMbps, &r.DownloadMbps, &r.BurstKBytes, &enabled, &r.Revision, &created, &updated)
	r.Enabled = enabled == 1
	r.CreatedAt = fromUnix(created)
	r.UpdatedAt = fromUnix(updated)
	return r, err
}

const ruleColumns = `id,line_id,mode,name,protocol,ingress_node_id,egress_node_id,listen_address,listen_port,relay_port,target_host,target_port,engine,upload_mbps,download_mbps,burst_kbytes,enabled,revision,created_at,updated_at`

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
	_, err = tx.ExecContext(ctx, `INSERT INTO forward_rules(`+ruleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET line_id=excluded.line_id,mode=excluded.mode,name=excluded.name,protocol=excluded.protocol,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,listen_port=excluded.listen_port,relay_port=excluded.relay_port,target_host=excluded.target_host,target_port=excluded.target_port,engine=excluded.engine,upload_mbps=excluded.upload_mbps,download_mbps=excluded.download_mbps,burst_kbytes=excluded.burst_kbytes,enabled=excluded.enabled,revision=excluded.revision,updated_at=excluded.updated_at`, r.ID, r.LineID, r.Mode, r.Name, r.Protocol, r.IngressNodeID, r.EgressNodeID, r.ListenAddress, r.ListenPort, r.RelayPort, r.TargetHost, r.TargetPort, r.Engine, r.UploadMbps, r.DownloadMbps, r.BurstKBytes, boolInt(r.Enabled), r.Revision, unix(r.CreatedAt), unix(r.UpdatedAt))
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
		if d.UploadBytes < 0 || d.DownloadBytes < 0 || d.UploadPackets < 0 || d.DownloadPackets < 0 {
			return errors.New("traffic counters cannot be negative")
		}
		var ruleExists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM forward_rules WHERE id=?`, d.RuleID).Scan(&ruleExists)
		if errors.Is(err, sql.ErrNoRows) {
			// An Agent can report one final counter sample after an administrator
			// deletes a rule. Ignore it so the sync can continue and the Agent can
			// receive the new revision that removes the stale local rule.
			continue
		}
		if err != nil {
			return err
		}
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
		dayBucket := trafficDayStart(time.Unix(bucket, 0)).Unix()
		_, err = tx.ExecContext(ctx, `INSERT INTO traffic_daily(rule_id,node_id,bucket,upload_bytes,download_bytes,upload_packets,download_packets) VALUES(?,?,?,?,?,?,?) ON CONFLICT(rule_id,node_id,bucket) DO UPDATE SET upload_bytes=upload_bytes+excluded.upload_bytes,download_bytes=download_bytes+excluded.download_bytes,upload_packets=upload_packets+excluded.upload_packets,download_packets=download_packets+excluded.download_packets`, d.RuleID, nodeID, dayBucket, d.UploadBytes, d.DownloadBytes, d.UploadPackets, d.DownloadPackets)
		if err != nil {
			return err
		}
	}
	today := time.Now().UTC().Truncate(24 * time.Hour).Unix()
	previousPrune := s.lastTrafficPrune.Load()
	if previousPrune != today && s.lastTrafficPrune.CompareAndSwap(previousPrune, today) {
		if _, err = tx.ExecContext(ctx, `DELETE FROM traffic_minute WHERE bucket<?`, time.Now().UTC().Add(-7*24*time.Hour).Unix()); err != nil {
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
	query := `SELECT (bucket/3600)*3600,SUM(upload_bytes),SUM(download_bytes),SUM(upload_packets),SUM(download_packets) FROM traffic_minute WHERE bucket>=?`
	args := []any{since.Unix()}
	if ruleID != "" {
		query += ` AND rule_id=?`
		args = append(args, ruleID)
	}
	query += ` GROUP BY (bucket/3600)*3600 ORDER BY (bucket/3600)*3600`
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

func (s *Store) DailyTraffic(ctx context.Context, ruleID string, since time.Time) ([]domain.TrafficPoint, error) {
	query := `SELECT bucket,SUM(upload_bytes),SUM(download_bytes),SUM(upload_packets),SUM(download_packets) FROM traffic_daily WHERE bucket>=?`
	args := []any{trafficDayStart(since).Unix()}
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
		var bucket int64
		if err := rows.Scan(&bucket, &p.UploadBytes, &p.DownloadBytes, &p.UploadPackets, &p.DownloadPackets); err != nil {
			return nil, err
		}
		p.Bucket = fromUnix(bucket)
		out = append(out, p)
	}
	return out, rows.Err()
}

func trafficPeriodStarts(now time.Time) (today, week, month, quarter int64) {
	now = now.In(trafficLocation)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, trafficLocation)
	weekday := (int(day.Weekday()) + 6) % 7
	weekStart := day.AddDate(0, 0, -weekday)
	monthStart := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, trafficLocation)
	quarterMonth := time.Month(((int(day.Month()) - 1) / 3 * 3) + 1)
	quarterStart := time.Date(day.Year(), quarterMonth, 1, 0, 0, 0, 0, trafficLocation)
	return day.Unix(), weekStart.Unix(), monthStart.Unix(), quarterStart.Unix()
}

func trafficDayStart(t time.Time) time.Time {
	local := t.In(trafficLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, trafficLocation)
}

func (s *Store) RuleTrafficSummaries(ctx context.Context) ([]domain.RuleTrafficSummary, error) {
	now := time.Now().UTC()
	today, week, month, quarter := trafficPeriodStarts(now)
	currentMinute := now.Truncate(time.Minute)
	elapsed := int64(now.Sub(currentMinute).Seconds())
	if elapsed < 10 {
		elapsed = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id,
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.upload_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.download_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.upload_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.download_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.upload_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.download_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.upload_bytes ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.bucket>=? THEN d.download_bytes ELSE 0 END),0),
			COALESCE(SUM(d.upload_bytes),0), COALESCE(SUM(d.download_bytes),0),
			COALESCE((SELECT SUM(m.upload_bytes) FROM traffic_minute m WHERE m.rule_id=r.id AND m.bucket=?),0),
			COALESCE((SELECT SUM(m.download_bytes) FROM traffic_minute m WHERE m.rule_id=r.id AND m.bucket=?),0)
		FROM forward_rules r
		LEFT JOIN traffic_daily d ON d.rule_id=r.id
		GROUP BY r.id
		ORDER BY r.created_at DESC`, today, today, week, week, month, month, quarter, quarter, currentMinute.Unix(), currentMinute.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RuleTrafficSummary
	for rows.Next() {
		var item domain.RuleTrafficSummary
		var currentUpload, currentDownload int64
		if err := rows.Scan(&item.RuleID, &item.TodayUploadBytes, &item.TodayDownloadBytes, &item.WeekUploadBytes, &item.WeekDownloadBytes, &item.MonthUploadBytes, &item.MonthDownloadBytes, &item.QuarterUploadBytes, &item.QuarterDownloadBytes, &item.TotalUploadBytes, &item.TotalDownloadBytes, &currentUpload, &currentDownload); err != nil {
			return nil, err
		}
		item.UploadBytesPerSecond = currentUpload / elapsed
		item.DownloadBytesPerSecond = currentDownload / elapsed
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) Summary(ctx context.Context) (domain.DashboardSummary, error) {
	var d domain.DashboardSummary
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN last_seen_at>=? THEN 1 ELSE 0 END),0) FROM nodes`, time.Now().Add(-45*time.Second).Unix()).Scan(&d.TotalNodes, &d.OnlineNodes); err != nil {
		return d, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(enabled),0) FROM forward_rules`).Scan(&d.TotalRules, &d.EnabledRules); err != nil {
		return d, err
	}
	now := time.Now().UTC()
	today, week, month, quarter := trafficPeriodStarts(now)
	if err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN bucket>=? THEN upload_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN download_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN upload_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN download_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN upload_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN download_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN upload_bytes ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN bucket>=? THEN download_bytes ELSE 0 END),0)
		FROM traffic_daily`, today, today, week, week, month, month, quarter, quarter).Scan(
		&d.TodayUpload, &d.TodayDownload, &d.WeekUpload, &d.WeekDownload,
		&d.MonthUpload, &d.MonthDownload, &d.QuarterUpload, &d.QuarterDownload,
	); err != nil {
		return d, err
	}
	points, err := s.Traffic(ctx, "", time.Now().UTC().Add(-24*time.Hour))
	if points == nil {
		points = []domain.TrafficPoint{}
	}
	d.RecentTraffic = points
	return d, err
}

func (s *Store) audit(ctx context.Context, action, kind, id, detail string) {
	_, _ = s.db.ExecContext(ctx, `INSERT INTO audit_logs(action,resource_type,resource_id,detail,created_at) VALUES(?,?,?,?,?)`, action, kind, id, detail, time.Now().Unix())
}
