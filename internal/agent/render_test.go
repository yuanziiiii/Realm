package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"relaypanel/internal/domain"
)

func TestRenderPlanBuildsTwoHopNATCountersAndLimits(t *testing.T) {
	node := domain.Node{ID: "node_in", Name: "入口", PublicInterface: "eth0", PrivateInterface: "wg0", PrivateAddress: "10.24.0.2"}
	rule := domain.ForwardRule{ID: "rule_demo", Name: "游戏", Protocol: "both", IngressNodeID: "node_in", EgressNodeID: "node_out", ListenPort: 24444, RelayPort: 32444, TargetHost: "192.0.2.88", TargetPort: 24444, Engine: "nftables", UploadMbps: 30, DownloadMbps: 100, BurstKBytes: 512, Enabled: true}
	deployments := []domain.Deployment{{Rule: rule, Role: domain.NodeRoleIngress}}
	plan, err := RenderPlan(node, deployments, true)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = FinalizePlan(plan, deployments, map[string]domain.Node{"node_out": {ID: "node_out", Name: "出口", PrivateAddress: "10.24.0.3"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"counter rp_demo_up", "counter rp_demo_down", "tcp dport 24444", "udp dport 24444", "dnat ip to 10.24.0.3:32444", "masquerade"} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("NFT script missing %q\n%s", want, plan.NFTScript)
		}
	}
	joined := ""
	for _, cmd := range plan.TC {
		joined += cmd.Name + " " + strings.Join(cmd.Args, " ") + "\n"
	}
	for _, want := range []string{"dev eth0 root handle 7a1: htb", "rate 100mbit", "dev wg0 root handle 7a1: htb", "rate 30mbit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tc plan missing %q\n%s", want, joined)
		}
	}
}

func TestRenderExitOnlyNFTOwnsCountersAndUsesPrivateListener(t *testing.T) {
	node := domain.Node{
		ID:               "node_out",
		Name:             "香港出口",
		PublicAddress:    "198.51.100.24",
		PrivateAddress:   "10.24.0.3",
		PublicInterface:  "eth0",
		PrivateInterface: "wg0",
	}
	rule := domain.ForwardRule{
		ID: "rule_exit", Mode: domain.ForwardModeExitOnly, Name: "仅出口接管",
		Protocol: "both", IngressNodeID: node.ID, EgressNodeID: node.ID,
		ListenAddress: "10.24.0.3", ListenPort: 24444, RelayPort: 24444,
		TargetHost: "192.0.2.88", TargetPort: 443, Engine: "nftables",
		UploadMbps: 30, DownloadMbps: 100, Enabled: true,
	}
	plan, err := RenderPlan(node, []domain.Deployment{{Rule: rule, Role: domain.NodeRoleEgress}}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"counter rp_exit_up", "counter rp_exit_down",
		`iifname "wg0" ip daddr 10.24.0.3 tcp dport 24444`,
		"dnat ip to 192.0.2.88:443",
		"snat ip to 198.51.100.24",
	} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("NFT script missing %q\n%s", want, plan.NFTScript)
		}
	}
	if strings.Contains(plan.NFTScript, "__NODE_") {
		t.Fatalf("exit-only plan unexpectedly contains an ingress placeholder:\n%s", plan.NFTScript)
	}
	if len(plan.IngressRuleIDs) != 1 || plan.IngressRuleIDs[0] != rule.ID {
		t.Fatalf("exit-only egress must report traffic counters, got %#v", plan.IngressRuleIDs)
	}
	joined := ""
	for _, cmd := range plan.TC {
		joined += cmd.Name + " " + strings.Join(cmd.Args, " ") + "\n"
	}
	for _, want := range []string{"dev eth0 root handle 7a1: htb", "rate 30mbit", "dev wg0 root handle 7a1: htb", "rate 100mbit"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tc plan missing %q\n%s", want, joined)
		}
	}
}

func TestRenderExitOnlyRealmBindsPrivateAndPublicAddresses(t *testing.T) {
	node := domain.Node{
		ID: "node_out", PublicAddress: "198.51.100.24", PrivateAddress: "10.24.0.3",
		PublicInterface: "eth0", PrivateInterface: "wg0",
	}
	rule := domain.ForwardRule{
		ID: "rule_realm", Mode: domain.ForwardModeExitOnly, Name: "Realm 仅出口",
		Protocol: "tcp", IngressNodeID: node.ID, EgressNodeID: node.ID,
		ListenAddress: "10.24.0.3", ListenPort: 24444, RelayPort: 24444,
		TargetHost: "192.0.2.88", TargetPort: 443, Engine: "realm", Enabled: true,
	}
	plan, err := RenderPlan(node, []domain.Deployment{{Rule: rule, Role: domain.NodeRoleEgress}}, true)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Endpoints []struct {
			Listen    string `json:"listen"`
			Remote    string `json:"remote"`
			Through   string `json:"through"`
			Interface string `json:"interface"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(plan.RealmConfig, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 1 {
		t.Fatalf("expected one endpoint, got %#v", config.Endpoints)
	}
	got := config.Endpoints[0]
	if got.Listen != "10.24.0.3:24444" || got.Remote != "192.0.2.88:443" || got.Through != "198.51.100.24" || got.Interface != "eth0" {
		t.Fatalf("unexpected Realm endpoint: %+v", got)
	}
}

func TestCounterSnapshotsAreCumulative(t *testing.T) {
	snapshots := CounterSnapshots([]string{"rule_demo"}, map[string][2]int64{"rp_demo_up": {1000, 10}, "rp_demo_down": {2000, 20}})
	if len(snapshots) != 1 {
		t.Fatalf("got %d snapshots", len(snapshots))
	}
	got := snapshots[0]
	if !got.Cumulative || got.UploadBytes != 1000 || got.DownloadBytes != 2000 {
		t.Fatalf("unexpected snapshot: %+v", got)
	}
}
