package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"relaypanel/internal/domain"
	"relaypanel/internal/store"
)

func TestSaveRuleUpdatePreservesRelayPortAndChangesFields(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	original, err := st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Name: "旧目标", Mode: line.Mode, Protocol: "both", IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress, ListenPort: 10000, RelayPort: 31000, TargetHost: "192.0.2.1", TargetPort: 80, Engine: line.Engine, BurstKBytes: 512, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"line_id": line.ID, "name": "新目标", "protocol": "tcp", "listen_port": 10001,
		"target_host": "192.0.2.2", "target_port": 443, "upload_mbps": 20,
		"download_mbps": 50, "burst_kbytes": 256, "enabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/v1/rules/rule", bytes.NewReader(body))
	req.SetPathValue("id", original.ID)
	recorder := httptest.NewRecorder()
	(&Server{store: st}).saveRule(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", recorder.Code, recorder.Body.String())
	}
	var updated domain.ForwardRule
	if err := json.NewDecoder(recorder.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	if updated.ListenPort != 10001 || updated.TargetHost != "192.0.2.2" || updated.TargetPort != 443 || updated.Protocol != "tcp" {
		t.Fatalf("editable fields were not updated: %+v", updated)
	}
	if updated.UploadMbps != 20 || updated.DownloadMbps != 50 || updated.BurstKBytes != 256 {
		t.Fatalf("rate limits were not updated: %+v", updated)
	}
	if updated.RelayPort != original.RelayPort {
		t.Fatalf("relay port changed unexpectedly: got %d, want %d", updated.RelayPort, original.RelayPort)
	}
}

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

func TestCompleteLineValidatesRolesAndFillsListener(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "ingress", Name: "广州入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.24.0.2", CreatedAt: now},
		{ID: "egress", Name: "香港出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.24.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{store: st}

	dual := domain.Line{Mode: domain.ForwardModeDualManaged, IngressNodeID: "ingress", EgressNodeID: "egress"}
	if err := s.completeLine(ctx, &dual); err != nil {
		t.Fatal(err)
	}
	if dual.ListenAddress != "0.0.0.0" {
		t.Fatalf("unexpected dual listener: %q", dual.ListenAddress)
	}

	exitOnly := domain.Line{Mode: domain.ForwardModeExitOnly, EgressNodeID: "egress"}
	if err := s.completeLine(ctx, &exitOnly); err != nil {
		t.Fatal(err)
	}
	if exitOnly.IngressNodeID != "egress" || exitOnly.ListenAddress != "10.24.0.3" {
		t.Fatalf("unexpected exit-only line: %+v", exitOnly)
	}

	invalid := domain.Line{Mode: domain.ForwardModeDualManaged, IngressNodeID: "egress", EgressNodeID: "ingress"}
	if err := s.completeLine(ctx, &invalid); err == nil {
		t.Fatal("expected invalid node roles to be rejected")
	}
}

func TestCompleteSimpleRuleUsesConfiguredExitPortPool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out", Name: "NAT 出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "NAT 线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", RelayPortRange: "20000-20002", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "used", Name: "占用端口", Mode: domain.ForwardModeDualManaged, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10001, RelayPort: 20000, TargetHost: "192.0.2.1", TargetPort: 80, Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rule := domain.ForwardRule{LineID: line.ID, Mode: line.Mode, Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenPort: 10002, TargetHost: "192.0.2.2", TargetPort: 80}
	if err := s.completeSimpleRule(ctx, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.RelayPort != 20001 {
		t.Fatalf("expected first free NAT port 20001, got %d", rule.RelayPort)
	}
}

func TestLineUpdateMigratesExistingRulesToNewServers(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in-a", Name: "入口 A", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now},
		{ID: "out-a", Name: "出口 A", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now},
		{ID: "out-b", Name: "出口 B", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.4", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "线路", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in-a", EgressNodeID: "out-a", ListenAddress: "0.0.0.0", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Name: "网站", Mode: line.Mode, Protocol: "both", IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress, ListenPort: 24444, RelayPort: 32444, TargetHost: "192.0.2.88", TargetPort: 443, Engine: line.Engine, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	updated := line
	updated.EgressNodeID = "out-b"
	updated.RelayPortRange = "41000-41001"
	updated.Engine = "realm"
	s := &Server{store: st}
	rules, err := s.rulesForLineUpdate(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0].EgressNodeID != "out-b" || rules[0].RelayPort != 41000 || rules[0].Engine != "realm" {
		t.Fatalf("unexpected migrated rule: %+v", rules)
	}
	if _, err := st.SaveLineRules(ctx, updated, rules); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetRule(ctx, "rule")
	if err != nil || stored.EgressNodeID != "out-b" || stored.RelayPort != 41000 || stored.Engine != "realm" {
		t.Fatalf("rule migration was not stored: %+v, %v", stored, err)
	}
}
