package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

func TestCompleteSimpleRuleSelectsNodesAndRelayPort(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{{ID: "ingress", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now}, {ID: "egress", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now}} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{Mode: domain.ForwardModeDualManaged, Protocol: "both", ListenPort: 24444, TargetHost: "192.0.2.88", TargetPort: 24444}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.IngressNodeID != "ingress" || rule.EgressNodeID != "egress" {
		t.Fatalf("unexpected route: %+v", rule)
	}
	if rule.RelayPort < 30000 || rule.RelayPort >= 60000 {
		t.Fatalf("unexpected relay port: %d", rule.RelayPort)
	}
}

func TestCompleteExitOnlyRuleUsesEgressPrivateListener(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	node := domain.Node{ID: "egress", Name: "香港出口", Role: domain.NodeRoleEgress, PublicAddress: "198.51.100.24", PrivateAddress: "10.24.0.3", CreatedAt: now}
	if err := st.CreateNode(ctx, node, "hash"); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{Mode: domain.ForwardModeExitOnly, EgressNodeID: node.ID, Protocol: "both", ListenPort: 24444, TargetHost: "192.0.2.88", TargetPort: 443}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.IngressNodeID != node.ID || rule.EgressNodeID != node.ID || rule.ListenAddress != node.PrivateAddress || rule.RelayPort != rule.ListenPort {
		t.Fatalf("unexpected exit-only route: %+v", rule)
	}
}
