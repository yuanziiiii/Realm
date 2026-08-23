package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"relaypanel/internal/agent"
	"relaypanel/internal/domain"
)

// version is replaced by release builds through -ldflags "-X main.version=...".
var version = "dev"

type state struct {
	AppliedRevision     int64                     `json:"applied_revision"`
	AppliedConfigHash   string                    `json:"applied_config_hash,omitempty"`
	ApplyStatus         string                    `json:"apply_status"`
	ApplyError          string                    `json:"apply_error"`
	IngressRuleIDs      []string                  `json:"ingress_rule_ids"`
	Probes              []domain.LinkProbe        `json:"probes,omitempty"`
	TargetProbes        []domain.TargetProbe      `json:"target_probes,omitempty"`
	TargetProbesPending bool                      `json:"target_probes_pending,omitempty"`
	TargetProbedAt      time.Time                 `json:"target_probed_at,omitempty"`
	RateLimits          []domain.RateLimitStatus  `json:"rate_limits,omitempty"`
	NodeTraffic         *domain.NodeTrafficSample `json:"node_traffic,omitempty"`
	Connections         *domain.ConnectionReport  `json:"connections,omitempty"`
	AccessRules         []accessRuleSummary       `json:"access_rules,omitempty"`
}

// accessRuleSummary keeps the human-readable policy choices next to the
// applied Agent state. Expanded region ranges deliberately stay out of this
// structure: nftables already owns those ranges and zf only needs the original
// province/city selections for a readable diagnostic summary.
type accessRuleSummary struct {
	RuleID                        string   `json:"rule_id"`
	Name                          string   `json:"name"`
	ListenPort                    int      `json:"listen_port"`
	Protocol                      string   `json:"protocol"`
	PolicyEnabled                 bool     `json:"policy_enabled"`
	AllowCIDRs                    []string `json:"allow_cidrs,omitempty"`
	DenyCIDRs                     []string `json:"deny_cidrs,omitempty"`
	AllowRegions                  []string `json:"allow_regions,omitempty"`
	DenyRegions                   []string `json:"deny_regions,omitempty"`
	MaxTCPConnectionsPerIP        int      `json:"max_tcp_connections_per_ip,omitempty"`
	MaxTCPNewConnectionsPerMinute int      `json:"max_tcp_new_connections_per_minute,omitempty"`
	MaxUDPNewFlowsPerMinute       int      `json:"max_udp_new_flows_per_minute,omitempty"`
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
	var pendingTargetProbes []domain.TargetProbe
	if st.TargetProbesPending {
		pendingTargetProbes = st.TargetProbes
	}
	resp, err := client.Sync(ctx, domain.SyncRequest{AgentVersion: version, AppliedRevision: st.AppliedRevision, ApplyStatus: st.ApplyStatus, ApplyError: st.ApplyError, Network: network, Traffic: traffic, Probes: st.Probes, TargetProbes: pendingTargetProbes, RateLimits: st.RateLimits, NodeTraffic: st.NodeTraffic, Connections: st.Connections})
	if err != nil {
		return err
	}
	st.AccessRules = summarizeAccessRules(resp.Deployments)
	st.TargetProbesPending = false
	var linkProbes []domain.LinkProbe
	var targetProbes []domain.TargetProbe
	var connections domain.ConnectionReport
	var probeWG sync.WaitGroup
	probeWG.Add(2)
	go func() {
		defer probeWG.Done()
		linkProbes = agent.ProbeLinks(ctx, resp.Node, resp.ProbeTargets)
	}()
	go func() {
		defer probeWG.Done()
		connections = agent.CollectConnections(ctx, resp.Node, resp.Deployments)
	}()
	probeTargets := targetProbeDue(st.TargetProbedAt, cfg.TargetProbeInterval, time.Now())
	if probeTargets {
		probeWG.Add(1)
		go func() {
			defer probeWG.Done()
			targetProbes = agent.ProbeRuleTargets(ctx, resp.Node, resp.Deployments)
		}()
	}
	probeWG.Wait()
	st.Probes = linkProbes
	st.Connections = &connections
	if probeTargets {
		st.TargetProbes = targetProbes
		st.TargetProbesPending = len(targetProbes) > 0
		st.TargetProbedAt = time.Now().UTC()
	}
	trafficInterface := resp.Node.TrafficQuotaInterface
	if trafficInterface == "" {
		trafficInterface = resp.Node.PublicInterface
	}
	if sample, sampleErr := agent.ReadInterfaceTraffic(trafficInterface); sampleErr == nil {
		st.NodeTraffic = &sample
	} else {
		st.NodeTraffic = nil
	}
	if appliedConfigurationMatches(resp, st) && st.ApplyStatus == "normal" && executor.Healthy(ctx) {
		st.RateLimits = executor.RateLimitStatuses(ctx, resp.Node.ID)
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
	st.AppliedConfigHash = resp.ConfigHash
	st.ApplyStatus = "normal"
	st.ApplyError = ""
	st.IngressRuleIDs = plan.IngressRuleIDs
	st.RateLimits = executor.RateLimitStatuses(ctx, resp.Node.ID)
	return nil
}

func summarizeAccessRules(deployments []domain.Deployment) []accessRuleSummary {
	result := make([]accessRuleSummary, 0, len(deployments))
	for _, deployment := range deployments {
		policy := deployment.Rule.AccessPolicy
		clientFacing := deployment.Role == domain.NodeRoleIngress || deployment.Role == domain.NodeRoleBoth ||
			(deployment.Rule.Mode == domain.ForwardModeExitOnly && deployment.Role == domain.NodeRoleEgress)
		enabled := policy.Enabled || policy.MaxTCPConnectionsPerIP > 0 || policy.MaxTCPNewConnectionsMinute > 0 || policy.MaxUDPNewFlowsMinute > 0
		if !clientFacing || !enabled {
			continue
		}
		result = append(result, accessRuleSummary{
			RuleID: deployment.Rule.ID, Name: deployment.Rule.Name, ListenPort: deployment.Rule.ListenPort,
			Protocol: deployment.Rule.Protocol, PolicyEnabled: policy.Enabled,
			AllowCIDRs: append([]string(nil), policy.AllowCIDRs...), DenyCIDRs: append([]string(nil), policy.DenyCIDRs...),
			AllowRegions: append([]string(nil), policy.AllowRegions...), DenyRegions: append([]string(nil), policy.DenyRegions...),
			MaxTCPConnectionsPerIP:        policy.MaxTCPConnectionsPerIP,
			MaxTCPNewConnectionsPerMinute: policy.MaxTCPNewConnectionsMinute,
			MaxUDPNewFlowsPerMinute:       policy.MaxUDPNewFlowsMinute,
		})
	}
	return result
}

func appliedConfigurationMatches(resp domain.SyncResponse, st *state) bool {
	if resp.Revision != st.AppliedRevision {
		return false
	}
	// Empty hashes preserve compatibility with older controllers. When a new
	// controller starts returning a hash, an upgraded Agent performs one
	// reconciliation to establish the local fingerprint.
	return resp.ConfigHash == "" || (st.AppliedConfigHash != "" && resp.ConfigHash == st.AppliedConfigHash)
}

func targetProbeDue(last time.Time, interval time.Duration, now time.Time) bool {
	return last.IsZero() || interval <= 0 || !now.Before(last.Add(interval))
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
