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
	"time"

	"relaypanel/internal/domain"
)

type Executor struct {
	Apply        bool
	StateDir     string
	RealmBinary  string
	log          *slog.Logger
	mu           sync.Mutex
	realm        *exec.Cmd
	realmDone    chan struct{}
	realmHash    [32]byte
	realmWanted  bool
	tcInterfaces []string
	rateLimits   []RateLimitSpec
	reconciled   bool
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
		e.mu.Lock()
		e.reconciled = true
		e.realmWanted = len(p.RealmConfig) > 0
		e.tcInterfaces = managedTCInterfaces(p.TC)
		e.rateLimits = append([]RateLimitSpec(nil), p.RateLimits...)
		e.mu.Unlock()
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
	if err := reconcileForwardingCompat(ctx, p.ForwardMarks); err != nil {
		rollbackNFT(ctx, previous, hadPrevious)
		return err
	}
	for _, c := range p.TC {
		cmd := exec.CommandContext(ctx, c.Name, c.Args...)
		out, err := cmd.CombinedOutput()
		if err != nil && isManagedQdiscAlreadyPresent(ctx, c, out) {
			continue
		}
		if err != nil {
			replaced, replaceErr := replaceDefaultRootQdisc(ctx, c, out)
			if replaceErr != nil {
				rollbackNFT(ctx, previous, hadPrevious)
				return replaceErr
			}
			if replaced {
				continue
			}
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
	e.mu.Lock()
	e.reconciled = true
	e.realmWanted = len(p.RealmConfig) > 0
	e.tcInterfaces = managedTCInterfaces(p.TC)
	e.rateLimits = append([]RateLimitSpec(nil), p.RateLimits...)
	e.mu.Unlock()
	return nil
}

// RateLimitStatuses verifies the concrete tc class and filter for every
// configured rule direction. A root HTB qdisc alone is not enough: a missing
// child class or filter silently sends traffic through the unlimited default
// class, which is the failure mode administrators need to see.
func (e *Executor) RateLimitStatuses(ctx context.Context, nodeID string) []domain.RateLimitStatus {
	e.mu.Lock()
	specs := append([]RateLimitSpec(nil), e.rateLimits...)
	apply := e.Apply
	e.mu.Unlock()
	checkedAt := time.Now().UTC()
	statuses := make([]domain.RateLimitStatus, 0, len(specs))
	for _, spec := range specs {
		status := domain.RateLimitStatus{
			RuleID: spec.RuleID, NodeID: nodeID, Direction: spec.Direction,
			Interface: spec.Interface, ConfiguredMbps: spec.ConfiguredMbps,
			CheckedAt: checkedAt,
		}
		if !apply {
			status.Installed = true
			statuses = append(statuses, status)
			continue
		}
		classOutput, classErr := exec.CommandContext(ctx, "tc", "class", "show", "dev", spec.Interface).CombinedOutput()
		if classErr != nil {
			status.Error = fmt.Sprintf("读取 tc class 失败: %s", strings.TrimSpace(string(classOutput)))
			statuses = append(statuses, status)
			continue
		}
		if !strings.Contains(string(classOutput), "class htb "+spec.ClassID) {
			status.Error = "限速 class 未安装"
			statuses = append(statuses, status)
			continue
		}
		filterOutput, filterErr := exec.CommandContext(ctx, "tc", "filter", "show", "dev", spec.Interface, "parent", "7a1:").CombinedOutput()
		if filterErr != nil {
			status.Error = fmt.Sprintf("读取 tc filter 失败: %s", strings.TrimSpace(string(filterOutput)))
			statuses = append(statuses, status)
			continue
		}
		if !strings.Contains(string(filterOutput), "flowid "+spec.ClassID) {
			status.Error = "限速 filter 未安装"
			statuses = append(statuses, status)
			continue
		}
		status.Installed = true
		statuses = append(statuses, status)
	}
	return statuses
}

// Healthy verifies runtime state instead of trusting the revision persisted on
// disk. nftables tables and Realm processes do not survive every reboot/crash,
// so a matching controller revision alone is not proof that forwarding exists.
func (e *Executor) Healthy(ctx context.Context) bool {
	e.mu.Lock()
	reconciled, realmWanted, realm, realmDone := e.reconciled, e.realmWanted, e.realm, e.realmDone
	tcInterfaces := append([]string(nil), e.tcInterfaces...)
	e.mu.Unlock()
	if !reconciled {
		return false
	}
	if !e.Apply {
		return true
	}
	if runtime.GOOS != "linux" {
		return false
	}
	nftOutput, err := exec.CommandContext(ctx, "nft", "list", "table", "inet", "relay_panel").Output()
	if err != nil {
		return false
	}
	for _, chain := range []string{"chain prerouting", "chain postrouting", "chain forward_stats", "chain input_stats", "chain output_stats"} {
		if !bytes.Contains(nftOutput, []byte(chain)) {
			return false
		}
	}
	if iptables, err := exec.LookPath("iptables"); err == nil && exec.CommandContext(ctx, iptables, "-w", "-C", "FORWARD", "-j", "RELAY-PANEL").Run() != nil {
		return false
	}
	for _, iface := range tcInterfaces {
		output, err := exec.CommandContext(ctx, "tc", "qdisc", "show", "dev", iface).Output()
		if err != nil || !strings.Contains(string(output), "qdisc htb 7a1:") {
			return false
		}
	}
	if !realmWanted {
		return true
	}
	if realm == nil || realmDone == nil {
		return false
	}
	select {
	case <-realmDone:
		return false
	default:
		return true
	}
}

func managedTCInterfaces(commands []Command) []string {
	seen := map[string]bool{}
	var interfaces []string
	for _, command := range commands {
		if command.Name != "tc" || len(command.Args) < 5 || command.Args[0] != "qdisc" {
			continue
		}
		for i, arg := range command.Args {
			if arg == "dev" && i+1 < len(command.Args) && !seen[command.Args[i+1]] {
				seen[command.Args[i+1]] = true
				interfaces = append(interfaces, command.Args[i+1])
			}
		}
	}
	return interfaces
}

// reconcileForwardingCompat inserts a narrowly scoped iptables chain ahead of
// Docker's FORWARD policy. Docker commonly changes that policy to DROP, which
// otherwise blocks packets that were already DNATed by the managed nftables
// rules. Connection marks make the exception apply only to Relay Panel flows,
// including their return packets.
func reconcileForwardingCompat(ctx context.Context, marks []uint32) error {
	iptables, err := exec.LookPath("iptables")
	if err != nil {
		return nil
	}
	run := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, iptables, append([]string{"-w"}, args...)...).CombinedOutput()
	}
	const chain = "RELAY-PANEL"
	if _, err := run("-S", chain); err != nil {
		if out, createErr := run("-N", chain); createErr != nil {
			return fmt.Errorf("create forwarding compatibility chain: %w: %s", createErr, bytes.TrimSpace(out))
		}
	}
	if _, err := run("-C", "FORWARD", "-j", chain); err != nil {
		if out, insertErr := run("-I", "FORWARD", "1", "-j", chain); insertErr != nil {
			return fmt.Errorf("attach forwarding compatibility chain: %w: %s", insertErr, bytes.TrimSpace(out))
		}
	}
	if out, err := run("-F", chain); err != nil {
		return fmt.Errorf("flush forwarding compatibility chain: %w: %s", err, bytes.TrimSpace(out))
	}
	seen := make(map[uint32]bool, len(marks))
	for _, mark := range marks {
		if mark == 0 || seen[mark] {
			continue
		}
		seen[mark] = true
		value := fmt.Sprintf("0x%x/0xffffffff", mark)
		if out, err := run("-A", chain, "-m", "connmark", "--mark", value, "-j", "ACCEPT"); err != nil {
			return fmt.Errorf("allow managed forwarding mark %s: %w: %s", value, err, bytes.TrimSpace(out))
		}
	}
	if out, err := run("-A", chain, "-j", "RETURN"); err != nil {
		return fmt.Errorf("finalize forwarding compatibility chain: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func replaceDefaultRootQdisc(ctx context.Context, c Command, commandOutput []byte) (bool, error) {
	if c.Name != "tc" || len(c.Args) < 5 || c.Args[0] != "qdisc" || c.Args[1] != "add" || !strings.Contains(string(commandOutput), "File exists") {
		return false, nil
	}
	var iface string
	for i, arg := range c.Args {
		if arg == "dev" && i+1 < len(c.Args) {
			iface = c.Args[i+1]
			break
		}
	}
	if iface == "" {
		return false, nil
	}
	listed, err := exec.CommandContext(ctx, "tc", "qdisc", "show", "dev", iface).Output()
	if err != nil || !isReplaceableDefaultQdisc(string(listed)) {
		return false, nil
	}
	args := append([]string(nil), c.Args...)
	args[1] = "replace"
	out, err := exec.CommandContext(ctx, c.Name, args...).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("replace default qdisc on %s: %w: %s", iface, err, bytes.TrimSpace(out))
	}
	return true, nil
}

func isReplaceableDefaultQdisc(listed string) bool {
	for _, line := range strings.Split(listed, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(" "+line+" ", " root ") {
			continue
		}
		for _, prefix := range []string{"qdisc pfifo_fast 0:", "qdisc fq_codel 0:", "qdisc noqueue 0:", "qdisc mq 0:"} {
			if strings.HasPrefix(line, prefix) {
				return true
			}
		}
	}
	return false
}

func captureNFT(ctx context.Context) ([]byte, bool) {
	out, err := exec.CommandContext(ctx, "nft", "list", "table", "inet", "relay_panel").Output()
	return out, err == nil
}
func rollbackNFT(ctx context.Context, previous []byte, hadPrevious bool) {
	if hadPrevious {
		// `nft list table` includes the table declaration. Delete the failed
		// table before replaying that declaration; flushing leaves the table in
		// place and makes restoration fail with "File exists".
		_ = exec.CommandContext(ctx, "nft", "delete", "table", "inet", "relay_panel").Run()
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
		e.stopRealmLocked()
		return nil
	}
	if e.realm != nil && hash == e.realmHash && e.realmDone != nil {
		select {
		case <-e.realmDone:
			// Realm exited unexpectedly; start it again below.
		default:
			return nil
		}
	}
	if err := os.MkdirAll(e.StateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(e.StateDir, "realm.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		return err
	}
	e.stopRealmLocked()
	cmd := exec.Command(e.RealmBinary, "-c", path)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start realm: %w", err)
	}
	e.realm = cmd
	e.realmDone = make(chan struct{})
	e.realmHash = hash
	done := e.realmDone
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return nil
}

func (e *Executor) stopRealmLocked() {
	if e.realm != nil && e.realm.Process != nil {
		_ = e.realm.Process.Kill()
		if e.realmDone != nil {
			<-e.realmDone
		}
	}
	e.realm = nil
	e.realmDone = nil
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
