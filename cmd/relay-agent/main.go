package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"relaypanel/internal/agent"
	"relaypanel/internal/domain"
)

// version is replaced by release builds through -ldflags "-X main.version=...".
var version = "dev"

type state struct {
	AppliedRevision int64              `json:"applied_revision"`
	ApplyStatus     string             `json:"apply_status"`
	ApplyError      string             `json:"apply_error"`
	IngressRuleIDs  []string           `json:"ingress_rule_ids"`
	Probes          []domain.LinkProbe `json:"probes,omitempty"`
}

func main() {
	configPath := flag.String("config", "/etc/relay-agent/config.json", "agent configuration path")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		log.Error("load config", "error", err)
		os.Exit(1)
	}
	if cfg.ControllerURL == "" || cfg.NodeID == "" || cfg.Token == "" {
		log.Error("controller_url, node_id and token are required")
		os.Exit(1)
	}
	if err = os.MkdirAll(cfg.StateDir, 0700); err != nil {
		log.Error("create state directory", "error", err)
		os.Exit(1)
	}
	statePath := filepath.Join(cfg.StateDir, "state.json")
	st := loadState(statePath)
	client := agent.NewClient(cfg.ControllerURL, cfg.NodeID, cfg.Token)
	executor := agent.NewExecutor(cfg.Apply, cfg.StateDir, cfg.RealmBinary, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	network := agent.DetectNetwork(ctx)
	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	for {
		if err = cycle(ctx, cfg, client, executor, &st, network); err != nil {
			log.Warn("sync failed", "error", err)
		}
		saveState(statePath, st)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func cycle(ctx context.Context, cfg agent.Config, client *agent.Client, executor *agent.Executor, st *state, network domain.NetworkInfo) error {
	var traffic []domain.TrafficDelta
	if cfg.Apply {
		if counters, err := agent.ReadCounters(ctx); err == nil {
			traffic = agent.CounterSnapshots(st.IngressRuleIDs, counters)
			for i := range traffic {
				traffic[i].CapturedAt = time.Now().UTC()
			}
		}
	}
	resp, err := client.Sync(ctx, domain.SyncRequest{AgentVersion: version, AppliedRevision: st.AppliedRevision, ApplyStatus: st.ApplyStatus, ApplyError: st.ApplyError, Network: network, Traffic: traffic, Probes: st.Probes})
	if err != nil {
		return err
	}
	st.Probes = agent.ProbeLinks(ctx, resp.Node, resp.ProbeTargets)
	if resp.Revision == st.AppliedRevision && st.ApplyStatus == "normal" && executor.Healthy(ctx) {
		return nil
	}
	nodes := map[string]domain.Node{resp.Node.ID: resp.Node}
	for _, peer := range resp.Peers {
		nodes[peer.ID] = peer
	}
	plan, err := agent.RenderPlan(resp.Node, resp.Deployments, cfg.AllowQdiscReplace)
	if err == nil {
		plan, err = agent.FinalizePlan(plan, resp.Deployments, nodes)
	}
	if err == nil {
		err = executor.Reconcile(ctx, plan)
	}
	if err != nil {
		st.ApplyStatus = "failed"
		st.ApplyError = err.Error()
		return err
	}
	st.AppliedRevision = resp.Revision
	st.ApplyStatus = "normal"
	st.ApplyError = ""
	st.IngressRuleIDs = plan.IngressRuleIDs
	return nil
}

func loadState(path string) state {
	var s state
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}
func saveState(path string, s state) {
	b, _ := json.MarshalIndent(s, "", "  ")
	_ = os.WriteFile(path, b, 0600)
}
