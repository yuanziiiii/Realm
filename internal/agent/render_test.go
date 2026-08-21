package agent

import (
	"encoding/json"
	"fmt"
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
	for _, want := range []string{"counter rp_demo_up", "counter rp_demo_down", "tcp dport 24444", "udp dport 24444", "ct mark set", "dnat ip to 10.24.0.3:32444", "masquerade"} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("NFT script missing %q\n%s", want, plan.NFTScript)
		}
	}
	for _, want := range []string{
		fmt.Sprintf("ct mark %d ct direction original counter name rp_demo_up meta mark set %d", uploadMark(rule.ID), uploadMark(rule.ID)),
		fmt.Sprintf("ct mark %d ct direction reply counter name rp_demo_down meta mark set %d", uploadMark(rule.ID), downloadMark(rule.ID)),
	} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("NFT forwarding stats missing %q\n%s", want, plan.NFTScript)
		}
	}
	if strings.Contains(plan.NFTScript, "tcp dport 24444 counter name rp_demo_up") || strings.Contains(plan.NFTScript, "udp dport 24444 counter name rp_demo_up") {
		t.Fatalf("upload traffic must be counted per packet in forward_stats, not only on the first NAT packet:\n%s", plan.NFTScript)
	}
	joined := ""
	for _, cmd := range plan.TC {
		joined += cmd.Name + " " + strings.Join(cmd.Args, " ") + "\n"
	}
	for _, want := range []string{"dev eth0 root handle 7a1: htb", "rate 100mbit", "dev wg0 root handle 7a1: htb", "rate 30mbit", "quantum 60000"} {
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
		`oifname "eth0" ip daddr 192.0.2.88 tcp dport 443 masquerade`,
		fmt.Sprintf("ct mark %d ct direction original counter name rp_exit_up meta mark set %d", uploadMark(rule.ID), uploadMark(rule.ID)),
		"ct direction reply counter name rp_exit_down",
	} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("NFT script missing %q\n%s", want, plan.NFTScript)
		}
	}
	if strings.Contains(plan.NFTScript, "tcp dport 24444 counter name rp_exit_up") || strings.Contains(plan.NFTScript, "udp dport 24444 counter name rp_exit_up") {
		t.Fatalf("exit-only upload traffic must not be counted in the first-packet-only NAT chain:\n%s", plan.NFTScript)
	}
	if strings.Contains(plan.NFTScript, "__NODE_") {
		t.Fatalf("exit-only plan unexpectedly contains an ingress placeholder:\n%s", plan.NFTScript)
	}
	if len(plan.IngressRuleIDs) != 1 || plan.IngressRuleIDs[0] != rule.ID {
		t.Fatalf("exit-only egress must report traffic counters, got %#v", plan.IngressRuleIDs)
	}
	if len(plan.ForwardMarks) != 1 || plan.ForwardMarks[0] != uploadMark(rule.ID) {
		t.Fatalf("exit-only nftables rule must publish its connection mark, got %#v", plan.ForwardMarks)
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

func TestRenderExitOnlyRealmBindsPrivateAddressAndOutboundInterface(t *testing.T) {
	node := domain.Node{
		ID: "node_out", PublicAddress: "198.51.100.24", PrivateAddress: "10.24.0.3",
		PublicInterface: "eth0", PrivateInterface: "wg0",
	}
	rule := domain.ForwardRule{
		ID: "rule_realm", Mode: domain.ForwardModeExitOnly, Name: "Realm 仅出口",
		Protocol: "tcp", IngressNodeID: node.ID, EgressNodeID: node.ID,
		// Keep these deliberately different: an exit-only Realm deployment must
		// consistently inspect the actual egress listener (RelayPort).
		ListenAddress: "10.24.0.3", ListenPort: 11301, RelayPort: 24444,
		TargetHost: "landing.example.com", TargetPort: 443, Engine: "realm",
		UploadMbps: 30, DownloadMbps: 100, Enabled: true,
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
	if got.Listen != "10.24.0.3:24444" || got.Remote != "landing.example.com:443" || got.Through != "" || got.Interface != "eth0" {
		t.Fatalf("unexpected Realm endpoint: %+v", got)
	}
	joined := ""
	for _, cmd := range plan.TC {
		joined += cmd.Name + " " + strings.Join(cmd.Args, " ") + "\n"
	}
	for _, want := range []string{
		"qdisc add dev wg0 handle ffff: ingress",
		"filter replace dev wg0 parent ffff:",
		"flower ip_proto tcp dst_port 24444",
		"redirect dev ifb-relay0",
		"dev ifb-relay0 root handle 7a1: htb",
		"rate 30mbit",
		"dev wg0 root handle 7a1: htb",
		"rate 100mbit",
		"flower ip_proto tcp src_port 24444",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("tc plan missing %q\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "dst_port 11301") || strings.Contains(joined, "src_port 11301") {
		t.Fatalf("exit-only Realm tc plan matched the public entry port instead of the egress listener:\n%s", joined)
	}
	if len(plan.RateLimits) != 2 {
		t.Fatalf("expected upload and download limiter diagnostics, got %#v", plan.RateLimits)
	}
	for _, spec := range plan.RateLimits {
		if spec.ListenPort != 24444 {
			t.Fatalf("exit-only Realm limiter diagnostics use the wrong listener: %+v", spec)
		}
	}
	for _, want := range []string{
		"counter rp_realm_up", "counter rp_realm_down",
		`iifname "wg0" ip daddr 10.24.0.3 tcp dport 24444 counter name rp_realm_up`,
		`oifname "wg0" ip saddr 10.24.0.3 tcp sport 24444 counter name rp_realm_down`,
	} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("Realm traffic stats missing %q\n%s", want, plan.NFTScript)
		}
	}
	if strings.Contains(plan.NFTScript, "dport 11301") || strings.Contains(plan.NFTScript, "sport 11301") {
		t.Fatalf("exit-only Realm stats matched the public entry port instead of the egress listener:\n%s", plan.NFTScript)
	}
}

func TestRenderDualManagedRealmLimitsAndTrafficForTCPAndUDP(t *testing.T) {
	node := domain.Node{
		ID: "node_in", Name: "入口", PublicInterface: "eth0", PrivateInterface: "eth1",
	}
	rule := domain.ForwardRule{
		ID: "rule_realm_dual", Mode: domain.ForwardModeDualManaged, Name: "Realm 双端",
		Protocol: "both", IngressNodeID: node.ID, EgressNodeID: "node_out",
		ListenAddress: "0.0.0.0", ListenPort: 11301, RelayPort: 1301,
		TargetHost: "192.0.2.88", TargetPort: 36666, Engine: "realm",
		UploadMbps: 40, DownloadMbps: 80, Enabled: true,
	}
	deployments := []domain.Deployment{{Rule: rule, Role: domain.NodeRoleIngress}}
	plan, err := RenderPlan(node, deployments, false)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = FinalizePlan(plan, deployments, map[string]domain.Node{
		"node_out": {ID: "node_out", Name: "出口", PrivateAddress: "10.24.0.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"counter rp_realm_dual_up", "counter rp_realm_dual_down",
		`iifname "eth0" tcp dport 11301 counter name rp_realm_dual_up`,
		`iifname "eth0" udp dport 11301 counter name rp_realm_dual_up`,
		`oifname "eth0" tcp sport 11301 counter name rp_realm_dual_down`,
		`oifname "eth0" udp sport 11301 counter name rp_realm_dual_down`,
	} {
		if !strings.Contains(plan.NFTScript, want) {
			t.Errorf("Realm traffic plan missing %q\n%s", want, plan.NFTScript)
		}
	}
	if strings.Contains(plan.NFTScript, "dnat ip to") {
		t.Fatalf("Realm plan must not install nftables DNAT rules:\n%s", plan.NFTScript)
	}
	joined := ""
	for _, cmd := range plan.TC {
		joined += cmd.Name + " " + strings.Join(cmd.Args, " ") + "\n"
	}
	for _, want := range []string{
		"qdisc add dev eth0 handle ffff: ingress",
		"flower ip_proto tcp dst_port 11301",
		"flower ip_proto udp dst_port 11301",
		"redirect dev ifb-relay0",
		"dev ifb-relay0 root handle 7a1: htb",
		"rate 40mbit",
		"dev eth0 root handle 7a1: htb",
		"rate 80mbit",
		"flower ip_proto tcp src_port 11301",
		"flower ip_proto udp src_port 11301",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Realm tc plan missing %q\n%s", want, joined)
		}
	}
	if len(plan.RateLimits) != 2 {
		t.Fatalf("expected two Realm limiter specs, got %#v", plan.RateLimits)
	}
	for _, spec := range plan.RateLimits {
		if spec.Direction == "upload" && (spec.Interface != "ifb-relay0" || spec.SourceInterface != "eth0" || spec.ListenPort != 11301) {
			t.Fatalf("unexpected Realm upload limiter spec: %+v", spec)
		}
		if spec.Direction == "download" && (spec.Interface != "eth0" || spec.SourceInterface != "") {
			t.Fatalf("unexpected Realm download limiter spec: %+v", spec)
		}
	}
	var config struct {
		Endpoints []struct {
			Listen  string `json:"listen"`
			Remote  string `json:"remote"`
			Network struct {
				NoTCP  bool `json:"no_tcp"`
				UseUDP bool `json:"use_udp"`
			} `json:"network"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(plan.RealmConfig, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Endpoints) != 1 || config.Endpoints[0].Listen != "0.0.0.0:11301" || config.Endpoints[0].Remote != "10.24.0.3:1301" || config.Endpoints[0].Network.NoTCP || !config.Endpoints[0].Network.UseUDP {
		t.Fatalf("unexpected dual-managed Realm config: %+v", config.Endpoints)
	}
}

func TestRenderPlanWithoutLimitsHasNoTCCommands(t *testing.T) {
	for _, engine := range []string{"nftables", "realm"} {
		node := domain.Node{ID: "in", PublicInterface: "eth0", PrivateInterface: "eth1"}
		rule := domain.ForwardRule{ID: "rule_" + engine, Mode: domain.ForwardModeDualManaged, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenPort: 10000, RelayPort: 20000, TargetHost: "192.0.2.2", TargetPort: 80, Engine: engine, Enabled: true}
		plan, err := RenderPlan(node, []domain.Deployment{{Rule: rule, Role: domain.NodeRoleIngress}}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.TC) != 0 || len(plan.RateLimits) != 0 {
			t.Fatalf("%s plan without limits must not install tc objects: %+v", engine, plan.TC)
		}
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

func TestReplaceableDefaultQdiscDetection(t *testing.T) {
	for _, listed := range []string{
		"qdisc pfifo_fast 0: root refcnt 2 bands 3",
		"qdisc fq_codel 0: root refcnt 2 limit 10240p",
		"qdisc noqueue 0: root refcnt 2",
		"qdisc mq 0: root",
	} {
		if !isReplaceableDefaultQdisc(listed) {
			t.Errorf("expected default qdisc to be replaceable: %s", listed)
		}
	}
	if isReplaceableDefaultQdisc("qdisc cake 8010: root bandwidth 100Mbit") {
		t.Fatal("must not replace an administrator-managed root qdisc")
	}
}
