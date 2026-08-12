package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"relaypanel/internal/domain"
)

type Executor struct {
	Apply       bool
	StateDir    string
	RealmBinary string
	log         *slog.Logger
	mu          sync.Mutex
	realm       *exec.Cmd
	realmHash   [32]byte
}

func NewExecutor(apply bool, stateDir, realmBinary string, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.Default()
	}
	return &Executor{Apply: apply, StateDir: stateDir, RealmBinary: realmBinary, log: log}
}

func (e *Executor) Reconcile(ctx context.Context, p Plan) error {
	if !e.Apply {
		e.log.Info("dry-run plan", "nft_bytes", len(p.NFTScript), "tc_commands", len(p.TC), "realm", len(p.RealmConfig) > 0)
		return nil
	}
	if runtime.GOOS != "linux" {
		return errors.New("apply mode is supported only on Linux")
	}
	previous, hadPrevious := captureNFT(ctx)
	if err := ensureNFTTable(ctx); err != nil {
		return err
	}
	check := exec.CommandContext(ctx, "nft", "-c", "-f", "-")
	check.Stdin = strings.NewReader(p.NFTScript)
	if out, err := check.CombinedOutput(); err != nil {
		rollbackNFT(ctx, previous, hadPrevious)
		return fmt.Errorf("validate nftables: %w: %s", err, bytes.TrimSpace(out))
	}
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(p.NFTScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply nftables: %w: %s", err, bytes.TrimSpace(out))
	}
	for _, c := range p.TC {
		cmd := exec.CommandContext(ctx, c.Name, c.Args...)
		out, err := cmd.CombinedOutput()
		if err != nil && isManagedQdiscAlreadyPresent(ctx, c, out) {
			continue
		}
		if err != nil && !(c.Name == "ip" && strings.Contains(string(out), "File exists")) {
			rollbackNFT(ctx, previous, hadPrevious)
			return fmt.Errorf("run %s: %w: %s", c.Name, err, bytes.TrimSpace(out))
		}
	}
	if err := e.reconcileRealm(p.RealmConfig); err != nil {
		rollbackNFT(ctx, previous, hadPrevious)
		return err
	}
	return nil
}

func captureNFT(ctx context.Context) ([]byte, bool) {
	out, err := exec.CommandContext(ctx, "nft", "list", "table", "inet", "relay_panel").Output()
	return out, err == nil
}
func rollbackNFT(ctx context.Context, previous []byte, hadPrevious bool) {
	if hadPrevious {
		_ = exec.CommandContext(ctx, "nft", "flush", "table", "inet", "relay_panel").Run()
		cmd := exec.CommandContext(ctx, "nft", "-f", "-")
		cmd.Stdin = bytes.NewReader(previous)
		_ = cmd.Run()
	} else {
		_ = exec.CommandContext(ctx, "nft", "delete", "table", "inet", "relay_panel").Run()
	}
}

func isManagedQdiscAlreadyPresent(ctx context.Context, c Command, out []byte) bool {
	if c.Name != "tc" || len(c.Args) < 5 || c.Args[0] != "qdisc" || c.Args[1] != "add" || !strings.Contains(string(out), "File exists") {
		return false
	}
	var iface string
	for i, arg := range c.Args {
		if arg == "dev" && i+1 < len(c.Args) {
			iface = c.Args[i+1]
			break
		}
	}
	if iface == "" {
		return false
	}
	listed, err := exec.CommandContext(ctx, "tc", "qdisc", "show", "dev", iface).Output()
	return err == nil && strings.Contains(string(listed), "qdisc htb 7a1:")
}

func ensureNFTTable(ctx context.Context) error {
	if exec.CommandContext(ctx, "nft", "list", "table", "inet", "relay_panel").Run() == nil {
		return nil
	}
	out, err := exec.CommandContext(ctx, "nft", "add", "table", "inet", "relay_panel").CombinedOutput()
	if err != nil {
		return fmt.Errorf("create nft table: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func (e *Executor) reconcileRealm(config []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	hash := sha256.Sum256(config)
	if len(config) == 0 {
		if e.realm != nil && e.realm.Process != nil {
			_ = e.realm.Process.Kill()
			_, _ = e.realm.Process.Wait()
			e.realm = nil
		}
		return nil
	}
	if e.realm != nil && e.realm.ProcessState == nil && hash == e.realmHash {
		return nil
	}
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(e.StateDir, "realm.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		return err
	}
	if e.realm != nil && e.realm.Process != nil {
		_ = e.realm.Process.Kill()
		_, _ = e.realm.Process.Wait()
	}
	cmd := exec.Command(e.RealmBinary, "-c", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start realm: %w", err)
	}
	e.realm = cmd
	e.realmHash = hash
	go func() { _ = cmd.Wait() }()
	return nil
}

type nftCounterDoc struct {
	NFTables []struct {
		Counter *struct {
			Name    string `json:"name"`
			Packets int64  `json:"packets"`
			Bytes   int64  `json:"bytes"`
		} `json:"counter,omitempty"`
	} `json:"nftables"`
}

func ReadCounters(ctx context.Context) (map[string][2]int64, error) {
	out, err := exec.CommandContext(ctx, "nft", "-j", "list", "counters", "table", "inet", "relay_panel").Output()
	if err != nil {
		return nil, err
	}
	var doc nftCounterDoc
	if err = json.Unmarshal(out, &doc); err != nil {
		return nil, err
	}
	result := map[string][2]int64{}
	for _, item := range doc.NFTables {
		if item.Counter != nil {
			result[item.Counter.Name] = [2]int64{item.Counter.Bytes, item.Counter.Packets}
		}
	}
	return result, nil
}

func CounterSnapshots(ruleIDs []string, current map[string][2]int64) []domain.TrafficDelta {
	var snapshots []domain.TrafficDelta
	for _, id := range ruleIDs {
		upName, downName := counterName(id, "up"), counterName(id, "down")
		up, down := current[upName], current[downName]
		snapshots = append(snapshots, domain.TrafficDelta{RuleID: id, Cumulative: true, UploadBytes: up[0], UploadPackets: up[1], DownloadBytes: down[0], DownloadPackets: down[1]})
	}
	return snapshots
}
