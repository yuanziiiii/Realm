package agent

import (
	"context"
	"testing"

	"relaypanel/internal/domain"
)

func TestParsePingOutput(t *testing.T) {
	output := `3 packets transmitted, 3 received, 0% packet loss, time 2003ms
rtt min/avg/max/mdev = 12.100/13.250/14.800/0.700 ms`
	latency, loss, ok := parsePingOutput(output)
	if !ok || latency != 13.25 || loss != 0 {
		t.Fatalf("unexpected probe: latency=%v loss=%v ok=%v", latency, loss, ok)
	}

	latency, loss, ok = parsePingOutput("3 packets transmitted, 0 received, 100% packet loss")
	if !ok || latency != 0 || loss != 100 {
		t.Fatalf("unexpected failed probe: latency=%v loss=%v ok=%v", latency, loss, ok)
	}
}

func TestProbeRuleTargetsOnlyUsesEgressDeployments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	deployments := []domain.Deployment{
		{Role: domain.NodeRoleIngress, Rule: domain.ForwardRule{ID: "ingress-only", Enabled: true, TargetHost: "192.0.2.1", TargetPort: 80}},
		{Role: domain.NodeRoleEgress, Rule: domain.ForwardRule{ID: "egress", Enabled: true, TargetHost: "192.0.2.2", TargetPort: 443}},
		{Role: domain.NodeRoleEgress, Rule: domain.ForwardRule{ID: "realm-hostname", Enabled: true, TargetHost: "origin.example.com", TargetPort: 443}},
		{Role: domain.NodeRoleEgress, Rule: domain.ForwardRule{ID: "disabled", Enabled: false, TargetHost: "192.0.2.3", TargetPort: 53}},
	}
	probes := ProbeRuleTargets(ctx, domain.Node{ID: "exit"}, deployments)
	if len(probes) != 2 || probes[0].RuleID != "egress" || probes[0].NodeID != "exit" || probes[0].Address != "192.0.2.2" || probes[0].Port != 443 || probes[1].Address != "origin.example.com" {
		t.Fatalf("unexpected target probes: %+v", probes)
	}
}

func TestProbeLinksUsesExitPrivateAddressForDualManagedIngress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probes := ProbeLinks(ctx, domain.Node{ID: "in", Role: domain.NodeRoleIngress}, []domain.Node{
		{ID: "out", Role: domain.NodeRoleEgress, PrivateAddress: "10.24.0.3", PublicAddress: "198.51.100.3"},
	})
	if len(probes) != 1 || probes[0].IngressNodeID != "in" || probes[0].EgressNodeID != "out" || probes[0].Address != "10.24.0.3" {
		t.Fatalf("dual-managed probe must use the exit private address: %+v", probes)
	}
}
