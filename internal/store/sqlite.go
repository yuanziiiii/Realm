package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
			engine TEXT NOT NULL DEFAULT 'nftables', ingress_engine TEXT NOT NULL DEFAULT '',
			egress_engine TEXT NOT NULL DEFAULT '', egress_node_ids TEXT NOT NULL DEFAULT '[]',
			active_egress_node_id TEXT NOT NULL DEFAULT '', failover_enabled INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS link_probes (
			ingress_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			egress_node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			address TEXT NOT NULL DEFAULT '', latency_ms REAL NOT NULL DEFAULT 0,
			packet_loss REAL NOT NULL DEFAULT 100, success INTEGER NOT NULL DEFAULT 0,
			has_succeeded INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0, success_count INTEGER NOT NULL DEFAULT 0,
			checked_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(ingress_node_id, egress_node_id)
		)`,
		`CREATE TABLE IF NOT EXISTS forward_rules (
			id TEXT PRIMARY KEY, line_id TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT 'dual_managed', name TEXT NOT NULL, protocol TEXT NOT NULL,
			ingress_node_id TEXT NOT NULL REFERENCES nodes(id),
			egress_node_id TEXT NOT NULL REFERENCES nodes(id),
			listen_address TEXT NOT NULL DEFAULT '0.0.0.0', listen_port INTEGER NOT NULL,
			relay_port INTEGER NOT NULL, target_host TEXT NOT NULL, target_port INTEGER NOT NULL,
			engine TEXT NOT NULL DEFAULT 'nftables', ingress_engine TEXT NOT NULL DEFAULT '',
			egress_engine TEXT NOT NULL DEFAULT '', upload_mbps INTEGER NOT NULL DEFAULT 0,
			download_mbps INTEGER NOT NULL DEFAULT 0, burst_kbytes INTEGER NOT NULL DEFAULT 512,
			enabled INTEGER NOT NULL DEFAULT 1, revision INTEGER NOT NULL,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
			UNIQUE(ingress_node_id, listen_port, protocol),
			UNIQUE(egress_node_id, relay_port, protocol)
		)`,
		`CREATE TABLE IF NOT EXISTS target_probes (
			rule_id TEXT NOT NULL REFERENCES forward_rules(id) ON DELETE CASCADE,
			node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			address TEXT NOT NULL DEFAULT '', port INTEGER NOT NULL DEFAULT 0,
			latency_ms REAL NOT NULL DEFAULT 0, packet_loss REAL NOT NULL DEFAULT 100,
			success INTEGER NOT NULL DEFAULT 0, has_succeeded INTEGER NOT NULL DEFAULT 0,
			failure_count INTEGER NOT NULL DEFAULT 0, success_count INTEGER NOT NULL DEFAULT 0,
			tcp_checked INTEGER NOT NULL DEFAULT 0, tcp_success INTEGER NOT NULL DEFAULT 0,
			tcp_latency_ms REAL NOT NULL DEFAULT 0, tcp_error TEXT NOT NULL DEFAULT '',
			checked_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(rule_id, node_id)
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
		`CREATE INDEX IF NOT EXISTS idx_probes_checked ON link_probes(checked_at)`,
		`CREATE INDEX IF NOT EXISTS idx_target_probes_checked ON target_probes(checked_at)`,
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
	if err := s.ensureColumn(ctx, "forward_rules", "ingress_engine", `ALTER TABLE forward_rules ADD COLUMN ingress_engine TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "forward_rules", "egress_engine", `ALTER TABLE forward_rules ADD COLUMN egress_engine TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "relay_port_range", `ALTER TABLE lines ADD COLUMN relay_port_range TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "ingress_engine", `ALTER TABLE lines ADD COLUMN ingress_engine TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "egress_engine", `ALTER TABLE lines ADD COLUMN egress_engine TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "egress_node_ids", `ALTER TABLE lines ADD COLUMN egress_node_ids TEXT NOT NULL DEFAULT '[]'`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "active_egress_node_id", `ALTER TABLE lines ADD COLUMN active_egress_node_id TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "lines", "failover_enabled", `ALTER TABLE lines ADD COLUMN failover_enabled INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "link_probes", "has_succeeded", `ALTER TABLE link_probes ADD COLUMN has_succeeded INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_probes", "tcp_checked", `ALTER TABLE target_probes ADD COLUMN tcp_checked INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_probes", "tcp_success", `ALTER TABLE target_probes ADD COLUMN tcp_success INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_probes", "tcp_latency_ms", `ALTER TABLE target_probes ADD COLUMN tcp_latency_ms REAL NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "target_probes", "tcp_error", `ALTER TABLE target_probes ADD COLUMN tcp_error TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// Older releases stored a single engine. Preserve its exact value when the
	// two per-hop columns are introduced.
	if _, err := s.db.ExecContext(ctx, `UPDATE lines SET ingress_engine=engine WHERE ingress_engine=''`); err != nil {
		return fmt.Errorf("backfill line ingress engine: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE lines SET egress_engine=engine WHERE egress_engine=''`); err != nil {
		return fmt.Errorf("backfill line egress engine: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE forward_rules SET ingress_engine=engine WHERE ingress_engine=''`); err != nil {
		return fmt.Errorf("backfill rule ingress engine: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE forward_rules SET egress_engine=engine WHERE egress_engine=''`); err != nil {
		return fmt.Errorf("backfill rule egress engine: %w", err)
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

func (s *Store) SetSettings(ctx context.Context, values map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE nodes SET name=?,role=?,public_address=?,private_address=?,public_interface=?,private_interface=? WHERE id=?`, n.Name, n.Role, n.PublicAddress, n.PrivateAddress, n.PublicInterface, n.PrivateInterface, n.ID)
	if err != nil {
		return err
	}
	count, _ := res.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_probes WHERE ingress_node_id=? OR egress_node_id=?`, n.ID, n.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_probes WHERE node_id=?`, n.ID); err != nil {
		return err
	}
	// Interface and address changes alter rendered nftables, Realm and tc
	// configuration. Bump the global revision so every affected Agent
	// reconciles immediately instead of keeping a healthy but stale plan.
	if _, err := s.bumpRevision(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.audit(ctx, "update", "node", n.ID, n.Name)
	return nil
}

func (s *Store) DeleteNode(ctx context.Context, id string) error {
	lines, err := s.ListLines(ctx)
	if err != nil {
		return err
	}
	for _, line := range lines {
		for _, egressID := range line.EgressNodeIDs {
			if egressID == id {
				return errors.New("node is referenced by a line")
			}
		}
	}
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
	var egressJSON string
	var failover, enabled int
	var created, updated int64
	err := scanner.Scan(&line.ID, &line.Name, &line.Mode, &line.IngressNodeID, &line.EgressNodeID, &line.ListenAddress, &line.RelayPortRange, &line.Engine, &line.IngressEngine, &line.EgressEngine, &egressJSON, &line.ActiveEgressNodeID, &failover, &enabled, &created, &updated)
	if err == nil && egressJSON != "" {
		_ = json.Unmarshal([]byte(egressJSON), &line.EgressNodeIDs)
	}
	line.FailoverEnabled = failover == 1
	normalizeLineEgresses(&line)
	line.NormalizeEngines()
	line.Enabled = enabled == 1
	line.CreatedAt = fromUnix(created)
	line.UpdatedAt = fromUnix(updated)
	return line, err
}

const lineColumns = `id,name,mode,ingress_node_id,egress_node_id,listen_address,relay_port_range,engine,ingress_engine,egress_engine,egress_node_ids,active_egress_node_id,failover_enabled,enabled,created_at,updated_at`

func normalizeLineEgresses(line *domain.Line) {
	seen := map[string]bool{}
	ordered := make([]string, 0, len(line.EgressNodeIDs)+1)
	if line.EgressNodeID != "" {
		ordered = append(ordered, line.EgressNodeID)
		seen[line.EgressNodeID] = true
	}
	for _, id := range line.EgressNodeIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ordered = append(ordered, id)
		}
	}
	line.EgressNodeIDs = ordered
	if !line.FailoverEnabled || !seen[line.ActiveEgressNodeID] {
		line.ActiveEgressNodeID = line.EgressNodeID
	}
}

func lineArgs(line domain.Line) []any {
	normalizeLineEgresses(&line)
	line.NormalizeEngines()
	egressJSON, _ := json.Marshal(line.EgressNodeIDs)
	return []any{line.ID, line.Name, line.Mode, line.IngressNodeID, line.EgressNodeID, line.ListenAddress, line.RelayPortRange, line.Engine, line.IngressEngine, line.EgressEngine, string(egressJSON), line.ActiveEgressNodeID, boolInt(line.FailoverEnabled), boolInt(line.Enabled), unix(line.CreatedAt), unix(line.UpdatedAt)}
}

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
	normalizeLineEgresses(&line)
	line.NormalizeEngines()
	_, err := s.db.ExecContext(ctx, `INSERT INTO lines(`+lineColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,mode=excluded.mode,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,relay_port_range=excluded.relay_port_range,engine=excluded.engine,ingress_engine=excluded.ingress_engine,egress_engine=excluded.egress_engine,egress_node_ids=excluded.egress_node_ids,active_egress_node_id=excluded.active_egress_node_id,failover_enabled=excluded.failover_enabled,enabled=excluded.enabled,updated_at=excluded.updated_at`, lineArgs(line)...)
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
	normalizeLineEgresses(&line)
	line.NormalizeEngines()
	_, err = tx.ExecContext(ctx, `INSERT INTO lines(`+lineColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name,mode=excluded.mode,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,relay_port_range=excluded.relay_port_range,engine=excluded.engine,ingress_engine=excluded.ingress_engine,egress_engine=excluded.egress_engine,egress_node_ids=excluded.egress_node_ids,active_egress_node_id=excluded.active_egress_node_id,failover_enabled=excluded.failover_enabled,enabled=excluded.enabled,updated_at=excluded.updated_at`, lineArgs(line)...)
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
			rules[i].NormalizeEngines()
			_, err = tx.ExecContext(ctx, `UPDATE forward_rules SET mode=?,ingress_node_id=?,egress_node_id=?,listen_address=?,relay_port=?,engine=?,ingress_engine=?,egress_engine=?,revision=?,updated_at=? WHERE id=? AND line_id=?`, rules[i].Mode, rules[i].IngressNodeID, rules[i].EgressNodeID, rules[i].ListenAddress, rules[i].RelayPort, rules[i].Engine, rules[i].IngressEngine, rules[i].EgressEngine, revision, unix(now), rules[i].ID, line.ID)
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
	err := scanner.Scan(&r.ID, &r.LineID, &r.Mode, &r.Name, &r.Protocol, &r.IngressNodeID, &r.EgressNodeID, &r.ListenAddress, &r.ListenPort, &r.RelayPort, &r.TargetHost, &r.TargetPort, &r.Engine, &r.IngressEngine, &r.EgressEngine, &r.UploadMbps, &r.DownloadMbps, &r.BurstKBytes, &enabled, &r.Revision, &created, &updated)
	r.NormalizeEngines()
	r.Enabled = enabled == 1
	r.CreatedAt = fromUnix(created)
	r.UpdatedAt = fromUnix(updated)
	return r, err
}

const ruleColumns = `id,line_id,mode,name,protocol,ingress_node_id,egress_node_id,listen_address,listen_port,relay_port,target_host,target_port,engine,ingress_engine,egress_engine,upload_mbps,download_mbps,burst_kbytes,enabled,revision,created_at,updated_at`

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
	r.NormalizeEngines()
	_, err = tx.ExecContext(ctx, `INSERT INTO forward_rules(`+ruleColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET line_id=excluded.line_id,mode=excluded.mode,name=excluded.name,protocol=excluded.protocol,ingress_node_id=excluded.ingress_node_id,egress_node_id=excluded.egress_node_id,listen_address=excluded.listen_address,listen_port=excluded.listen_port,relay_port=excluded.relay_port,target_host=excluded.target_host,target_port=excluded.target_port,engine=excluded.engine,ingress_engine=excluded.ingress_engine,egress_engine=excluded.egress_engine,upload_mbps=excluded.upload_mbps,download_mbps=excluded.download_mbps,burst_kbytes=excluded.burst_kbytes,enabled=excluded.enabled,revision=excluded.revision,updated_at=excluded.updated_at`, r.ID, r.LineID, r.Mode, r.Name, r.Protocol, r.IngressNodeID, r.EgressNodeID, r.ListenAddress, r.ListenPort, r.RelayPort, r.TargetHost, r.TargetPort, r.Engine, r.IngressEngine, r.EgressEngine, r.UploadMbps, r.DownloadMbps, r.BurstKBytes, boolInt(r.Enabled), r.Revision, unix(r.CreatedAt), unix(r.UpdatedAt))
	if err != nil {
		return r, err
	}
	// Any rule edit may change the destination or active deployment. Do not
	// display a latency result measured against the previous configuration.
	if _, err = tx.ExecContext(ctx, `DELETE FROM target_probes WHERE rule_id=?`, r.ID); err != nil {
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
	rules, err := s.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	lines, err := s.ListLines(ctx)
	if err != nil {
		return nil, err
	}
	lineByID := make(map[string]domain.Line, len(lines))
	for _, line := range lines {
		lineByID[line.ID] = line
	}
	var out []domain.Deployment
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		line, managedFailover := lineByID[rule.LineID]
		managedFailover = managedFailover && line.Mode == domain.ForwardModeDualManaged && line.FailoverEnabled && len(line.EgressNodeIDs) > 1
		if managedFailover {
			if nodeID == rule.IngressNodeID {
				ingressRule := rule
				ingressRule.EgressNodeID = line.ActiveEgressNodeID
				ingressRule.Engine = ingressRule.EngineForRole(domain.NodeRoleIngress)
				out = append(out, domain.Deployment{Rule: ingressRule, Role: domain.NodeRoleIngress})
			}
			for _, egressID := range line.EgressNodeIDs {
				if nodeID == egressID {
					egressRule := rule
					egressRule.EgressNodeID = egressID
					egressRule.Engine = egressRule.EngineForRole(domain.NodeRoleEgress)
					out = append(out, domain.Deployment{Rule: egressRule, Role: domain.NodeRoleEgress})
					break
				}
			}
			continue
		}
		if rule.IngressNodeID != nodeID && rule.EgressNodeID != nodeID {
			continue
		}
		role := domain.NodeRoleEgress
		if rule.Mode != domain.ForwardModeExitOnly && rule.IngressNodeID == nodeID {
			role = domain.NodeRoleIngress
		}
		if rule.Mode != domain.ForwardModeExitOnly && rule.IngressNodeID == nodeID && rule.EgressNodeID == nodeID {
			role = domain.NodeRoleBoth
		}
		rule.Engine = rule.EngineForRole(role)
		out = append(out, domain.Deployment{Rule: rule, Role: role})
	}
	return out, nil
}

func (s *Store) ProbeTargetsForIngress(ctx context.Context, ingressNodeID string) ([]domain.Node, error) {
	lines, err := s.ListLines(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var targets []domain.Node
	for _, line := range lines {
		if !line.Enabled || line.Mode != domain.ForwardModeDualManaged || line.IngressNodeID != ingressNodeID {
			continue
		}
		for _, id := range line.EgressNodeIDs {
			if seen[id] {
				continue
			}
			node, _, err := s.GetNode(ctx, id)
			if err == nil {
				seen[id] = true
				targets = append(targets, node)
			}
		}
	}
	return targets, nil
}

func (s *Store) UpsertLinkProbes(ctx context.Context, ingressNodeID string, probes []domain.LinkProbe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, probe := range probes {
		if probe.EgressNodeID == "" || probe.EgressNodeID == ingressNodeID || probe.PacketLoss < 0 || probe.PacketLoss > 100 || probe.LatencyMS < 0 {
			continue
		}
		var failures, successes, hasSucceeded int
		err = tx.QueryRowContext(ctx, `SELECT failure_count,success_count,has_succeeded FROM link_probes WHERE ingress_node_id=? AND egress_node_id=?`, ingressNodeID, probe.EgressNodeID).Scan(&failures, &successes, &hasSucceeded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if probe.Success && probe.PacketLoss < 100 {
			failures = 0
			successes++
			hasSucceeded = 1
		} else {
			failures++
			successes = 0
		}
		checkedAt := probe.CheckedAt
		if checkedAt.IsZero() {
			checkedAt = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO link_probes(ingress_node_id,egress_node_id,address,latency_ms,packet_loss,success,has_succeeded,failure_count,success_count,checked_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(ingress_node_id,egress_node_id) DO UPDATE SET address=excluded.address,latency_ms=excluded.latency_ms,packet_loss=excluded.packet_loss,success=excluded.success,has_succeeded=excluded.has_succeeded,failure_count=excluded.failure_count,success_count=excluded.success_count,checked_at=excluded.checked_at`, ingressNodeID, probe.EgressNodeID, probe.Address, probe.LatencyMS, probe.PacketLoss, boolInt(probe.Success), hasSucceeded, failures, successes, unix(checkedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListLinkProbes(ctx context.Context) ([]domain.LinkProbe, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ingress_node_id,egress_node_id,address,latency_ms,packet_loss,success,has_succeeded,failure_count,success_count,checked_at FROM link_probes ORDER BY ingress_node_id,egress_node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var probes []domain.LinkProbe
	for rows.Next() {
		var probe domain.LinkProbe
		var success, hasSucceeded int
		var checkedAt int64
		if err := rows.Scan(&probe.IngressNodeID, &probe.EgressNodeID, &probe.Address, &probe.LatencyMS, &probe.PacketLoss, &success, &hasSucceeded, &probe.FailureCount, &probe.SuccessCount, &checkedAt); err != nil {
			return nil, err
		}
		probe.Success = success == 1
		probe.HasSucceeded = hasSucceeded == 1
		probe.CheckedAt = fromUnix(checkedAt)
		probes = append(probes, probe)
	}
	return probes, rows.Err()
}

func (s *Store) UpsertTargetProbes(ctx context.Context, nodeID string, probes []domain.TargetProbe) error {
	deployments, err := s.DeploymentsForNode(ctx, nodeID)
	if err != nil {
		return err
	}
	allowed := make(map[string]domain.ForwardRule, len(deployments))
	for _, deployment := range deployments {
		if deployment.Role == domain.NodeRoleEgress || deployment.Role == domain.NodeRoleBoth {
			allowed[deployment.Rule.ID] = deployment.Rule
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, probe := range probes {
		rule, ok := allowed[probe.RuleID]
		if !ok || probe.Address != rule.TargetHost || probe.Port != rule.TargetPort || probe.PacketLoss < 0 || probe.PacketLoss > 100 || probe.LatencyMS < 0 || probe.TCPLatencyMS < 0 {
			continue
		}
		// Reject delayed results for a previous rule destination. Values written
		// to the database still come from the controller-owned rule.
		probe.NodeID = nodeID
		probe.Address = rule.TargetHost
		probe.Port = rule.TargetPort
		if rule.Protocol == "udp" || !probe.TCPChecked {
			probe.TCPChecked = false
			probe.TCPSuccess = false
			probe.TCPLatencyMS = 0
			probe.TCPError = ""
		} else {
			switch probe.TCPError {
			case "", "timeout", "refused", "unreachable", "dns", "error":
			default:
				probe.TCPError = "error"
			}
			if probe.TCPSuccess {
				probe.TCPError = ""
			} else {
				probe.TCPLatencyMS = 0
				if probe.TCPError == "" {
					probe.TCPError = "error"
				}
			}
		}
		var failures, successes, hasSucceeded int
		err = tx.QueryRowContext(ctx, `SELECT failure_count,success_count,has_succeeded FROM target_probes WHERE rule_id=? AND node_id=?`, probe.RuleID, nodeID).Scan(&failures, &successes, &hasSucceeded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if probe.Success && probe.PacketLoss < 100 {
			failures = 0
			successes++
			hasSucceeded = 1
		} else {
			failures++
			successes = 0
		}
		checkedAt := probe.CheckedAt
		if checkedAt.IsZero() {
			checkedAt = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO target_probes(rule_id,node_id,address,port,latency_ms,packet_loss,success,has_succeeded,failure_count,success_count,tcp_checked,tcp_success,tcp_latency_ms,tcp_error,checked_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(rule_id,node_id) DO UPDATE SET address=excluded.address,port=excluded.port,latency_ms=excluded.latency_ms,packet_loss=excluded.packet_loss,success=excluded.success,has_succeeded=excluded.has_succeeded,failure_count=excluded.failure_count,success_count=excluded.success_count,tcp_checked=excluded.tcp_checked,tcp_success=excluded.tcp_success,tcp_latency_ms=excluded.tcp_latency_ms,tcp_error=excluded.tcp_error,checked_at=excluded.checked_at`, probe.RuleID, nodeID, probe.Address, probe.Port, probe.LatencyMS, probe.PacketLoss, boolInt(probe.Success), hasSucceeded, failures, successes, boolInt(probe.TCPChecked), boolInt(probe.TCPSuccess), probe.TCPLatencyMS, probe.TCPError, unix(checkedAt))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListTargetProbes(ctx context.Context) ([]domain.TargetProbe, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule_id,node_id,address,port,latency_ms,packet_loss,success,has_succeeded,failure_count,success_count,tcp_checked,tcp_success,tcp_latency_ms,tcp_error,checked_at FROM target_probes ORDER BY rule_id,node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var probes []domain.TargetProbe
	for rows.Next() {
		var probe domain.TargetProbe
		var success, hasSucceeded, tcpChecked, tcpSuccess int
		var checkedAt int64
		if err := rows.Scan(&probe.RuleID, &probe.NodeID, &probe.Address, &probe.Port, &probe.LatencyMS, &probe.PacketLoss, &success, &hasSucceeded, &probe.FailureCount, &probe.SuccessCount, &tcpChecked, &tcpSuccess, &probe.TCPLatencyMS, &probe.TCPError, &checkedAt); err != nil {
			return nil, err
		}
		probe.Success = success == 1
		probe.HasSucceeded = hasSucceeded == 1
		probe.TCPChecked = tcpChecked == 1
		probe.TCPSuccess = tcpSuccess == 1
		probe.CheckedAt = fromUnix(checkedAt)
		probes = append(probes, probe)
	}
	return probes, rows.Err()
}

func (s *Store) ReconcileFailover(ctx context.Context, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+lineColumns+` FROM lines WHERE enabled=1 AND mode=? AND failover_enabled=1`, domain.ForwardModeDualManaged)
	if err != nil {
		return false, err
	}
	var lines []domain.Line
	for rows.Next() {
		line, err := scanLine(rows)
		if err != nil {
			rows.Close()
			return false, err
		}
		lines = append(lines, line)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, line := range lines {
		if len(line.EgressNodeIDs) < 2 {
			continue
		}
		active := line.ActiveEgressNodeID
		if active == "" {
			active = line.EgressNodeID
		}
		activeHealthy, err := failoverCandidateHealthy(ctx, tx, line.IngressNodeID, active, now, true, 0)
		if err != nil {
			return false, err
		}
		desired := active
		if !activeHealthy {
			for _, candidate := range line.EgressNodeIDs {
				ready, err := failoverCandidateHealthy(ctx, tx, line.IngressNodeID, candidate, now, false, 2)
				if err != nil {
					return false, err
				}
				if ready {
					desired = candidate
					break
				}
			}
		} else {
			for _, candidate := range line.EgressNodeIDs {
				if candidate == active {
					break
				}
				ready, err := failoverCandidateHealthy(ctx, tx, line.IngressNodeID, candidate, now, false, 3)
				if err != nil {
					return false, err
				}
				if ready {
					desired = candidate
					break
				}
			}
		}
		if desired != active {
			if _, err := tx.ExecContext(ctx, `UPDATE lines SET active_egress_node_id=?,updated_at=? WHERE id=?`, desired, now.Unix(), line.ID); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if changed {
		if _, err := s.bumpRevision(ctx, tx); err != nil {
			return false, err
		}
	}
	return changed, tx.Commit()
}

func failoverCandidateHealthy(ctx context.Context, tx *sql.Tx, ingressNodeID, egressNodeID string, now time.Time, current bool, minimumSuccesses int) (bool, error) {
	var lastSeen int64
	if err := tx.QueryRowContext(ctx, `SELECT last_seen_at FROM nodes WHERE id=?`, egressNodeID).Scan(&lastSeen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if lastSeen < now.Add(-45*time.Second).Unix() {
		return false, nil
	}
	var loss float64
	var failures, successes, hasSucceeded int
	var checkedAt int64
	err := tx.QueryRowContext(ctx, `SELECT packet_loss,failure_count,success_count,has_succeeded,checked_at FROM link_probes WHERE ingress_node_id=? AND egress_node_id=?`, ingressNodeID, egressNodeID).Scan(&loss, &failures, &successes, &hasSucceeded, &checkedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return false, err
	}
	if checkedAt < now.Add(-45*time.Second).Unix() {
		return false, nil
	}
	// Some providers block ICMP. Until this path has answered at least once,
	// use the Agent heartbeat as health and never switch to it automatically.
	if hasSucceeded == 0 {
		return current, nil
	}
	if current {
		return failures < 3, nil
	}
	return loss < 100 && successes >= minimumSuccesses, nil
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
