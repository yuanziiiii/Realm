package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"relaypanel/internal/domain"
)

func TestCumulativeTrafficIsIdempotentAndHandlesReset(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, n := range []domain.Node{{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.0.0.2", CreatedAt: now}, {ID: "out", Name: "出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.0.0.3", CreatedAt: now}} {
		if err := st.CreateNode(ctx, n, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "rule", Mode: domain.ForwardModeDualManaged, Name: "测试", Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10000, RelayPort: 30000, TargetHost: "192.0.2.2", TargetPort: 80, Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	sample := domain.TrafficDelta{RuleID: "rule", CapturedAt: now, Cumulative: true, UploadBytes: 1000, DownloadBytes: 2000, UploadPackets: 10, DownloadPackets: 20}
	if err := st.AddTraffic(ctx, "in", []domain.TrafficDelta{sample}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTraffic(ctx, "in", []domain.TrafficDelta{sample}); err != nil {
		t.Fatal(err)
	}
	sample.UploadBytes = 100
	sample.DownloadBytes = 200
	sample.UploadPackets = 1
	sample.DownloadPackets = 2
	if err := st.AddTraffic(ctx, "in", []domain.TrafficDelta{sample}); err != nil {
		t.Fatal(err)
	}
	points, err := st.Traffic(ctx, "rule", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var up, down int64
	for _, p := range points {
		up += p.UploadBytes
		down += p.DownloadBytes
	}
	if up != 1100 || down != 2200 {
		t.Fatalf("expected reset-aware totals 1100/2200, got %d/%d", up, down)
	}
	summaries, err := st.RuleTrafficSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected one rule traffic summary, got %+v", summaries)
	}
	got := summaries[0]
	if got.RuleID != "rule" || got.TotalUploadBytes != 1100 || got.TotalDownloadBytes != 2200 || got.TodayUploadBytes != 1100 || got.TodayDownloadBytes != 2200 {
		t.Fatalf("unexpected rule traffic summary: %+v", got)
	}
}

func TestExitOnlyRuleCreatesOnlyEgressDeployment(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	node := domain.Node{ID: "out", Name: "香港出口", Role: domain.NodeRoleEgress, PublicAddress: "198.51.100.24", PrivateAddress: "10.24.0.3", CreatedAt: now}
	if err := st.CreateNode(ctx, node, "hash"); err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{
		ID: "rule_exit", Mode: domain.ForwardModeExitOnly, Name: "仅出口接管", Protocol: "both",
		IngressNodeID: node.ID, EgressNodeID: node.ID, ListenAddress: node.PrivateAddress,
		ListenPort: 24444, RelayPort: 24444, TargetHost: "192.0.2.88", TargetPort: 443,
		Engine: "nftables", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := st.DeploymentsForNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 1 || deployments[0].Role != domain.NodeRoleEgress {
		t.Fatalf("exit-only rule must produce one egress deployment, got %+v", deployments)
	}
}

func TestLineGroupsRulesAndCannotBeDeletedWhileReferenced(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, node := range []domain.Node{
		{ID: "in", Name: "广州入口", Role: domain.NodeRoleIngress, PrivateAddress: "10.24.0.2", CreatedAt: now},
		{ID: "out", Name: "香港出口", Role: domain.NodeRoleEgress, PrivateAddress: "10.24.0.3", CreatedAt: now},
	} {
		if err := st.CreateNode(ctx, node, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	line, err := st.SaveLine(ctx, domain.Line{ID: "line", Name: "广港专线", Mode: domain.ForwardModeDualManaged, IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", Engine: "nftables", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveRule(ctx, domain.ForwardRule{ID: "rule", LineID: line.ID, Mode: line.Mode, Name: "网站", Protocol: "tcp", IngressNodeID: line.IngressNodeID, EgressNodeID: line.EgressNodeID, ListenAddress: line.ListenAddress, ListenPort: 24444, RelayPort: 54444, TargetHost: "192.0.2.88", TargetPort: 443, Engine: line.Engine, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rules, err := st.ListRules(ctx)
	if err != nil || len(rules) != 1 || rules[0].LineID != line.ID {
		t.Fatalf("rule was not grouped under its line: %+v, %v", rules, err)
	}
	if err := st.DeleteLine(ctx, line.ID); err == nil {
		t.Fatal("expected referenced line deletion to fail")
	}
	if err := st.DeleteRule(ctx, "rule"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteLine(ctx, line.ID); err != nil {
		t.Fatal(err)
	}
}
