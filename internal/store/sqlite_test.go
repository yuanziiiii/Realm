package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relaypanel/internal/domain"
)

func TestSummarySupportsFreshEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	summary, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalNodes != 0 || summary.OnlineNodes != 0 || summary.TotalRules != 0 || summary.EnabledRules != 0 {
		t.Fatalf("unexpected empty summary: %+v", summary)
	}
	if summary.RecentTraffic == nil {
		t.Fatal("fresh summary must expose recent_traffic as an empty array, not null")
	}
	payload, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"recent_traffic":[]`) {
		t.Fatalf("unexpected empty summary JSON: %s", payload)
	}
}

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

func TestHeartbeatAutoFillsBlankNetworkWithoutOverwritingManualValues(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node := domain.Node{ID: "node", Name: "NAT 出口", Role: domain.NodeRoleEgress, PublicInterface: "eth0", PrivateInterface: "wg0", CreatedAt: time.Now().UTC()}
	if err := st.CreateNode(ctx, node, "hash"); err != nil {
		t.Fatal(err)
	}
	auto := domain.NetworkInfo{PublicAddress: "198.51.100.20", PrivateAddress: "10.24.0.3", PublicInterface: "ens3", PrivateInterface: "eth1"}
	if err := st.UpdateHeartbeat(ctx, node.ID, "0.3.0", "normal", "", 1, auto); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicAddress != auto.PublicAddress || got.PrivateAddress != auto.PrivateAddress || got.PublicInterface != auto.PublicInterface || got.PrivateInterface != auto.PrivateInterface {
		t.Fatalf("network fields were not auto-filled: %+v", got)
	}
	got.PublicAddress = "203.0.113.9"
	got.PrivateAddress = "172.16.1.9"
	got.PublicInterface = "manual0"
	got.PrivateInterface = "manual1"
	if err := st.UpdateNode(ctx, got); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateHeartbeat(ctx, node.ID, "0.3.0", "normal", "", 1, domain.NetworkInfo{PublicAddress: "198.51.100.99", PrivateAddress: "10.0.0.99", PublicInterface: "eth9", PrivateInterface: "eth8"}); err != nil {
		t.Fatal(err)
	}
	preserved, _, err := st.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.PublicAddress != "203.0.113.9" || preserved.PrivateAddress != "172.16.1.9" || preserved.PublicInterface != "manual0" || preserved.PrivateInterface != "manual1" {
		t.Fatalf("manual network values were overwritten: %+v", preserved)
	}
}

func TestTrafficKeepsDailyHistoryAndPrunesMinuteDetail(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, n := range []domain.Node{{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, CreatedAt: now}, {ID: "out", Name: "出口", Role: domain.NodeRoleEgress, CreatedAt: now}} {
		if err := st.CreateNode(ctx, n, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SaveRule(ctx, domain.ForwardRule{ID: "rule", Mode: domain.ForwardModeDualManaged, Name: "测试", Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10000, RelayPort: 30000, TargetHost: "192.0.2.2", TargetPort: 80, Engine: "nftables", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	old := domain.TrafficDelta{RuleID: "rule", CapturedAt: now.AddDate(0, 0, -10), UploadBytes: 100, DownloadBytes: 200}
	current := domain.TrafficDelta{RuleID: "rule", CapturedAt: now, UploadBytes: 300, DownloadBytes: 400}
	if err := st.AddTraffic(ctx, "in", []domain.TrafficDelta{old, current}); err != nil {
		t.Fatal(err)
	}
	var minuteRows int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM traffic_minute`).Scan(&minuteRows); err != nil {
		t.Fatal(err)
	}
	if minuteRows != 1 {
		t.Fatalf("expected only recent minute detail, got %d rows", minuteRows)
	}
	daily, err := st.DailyTraffic(ctx, "rule", now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 2 {
		t.Fatalf("expected old and current daily totals, got %+v", daily)
	}
	summaries, err := st.RuleTrafficSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].TotalUploadBytes != 400 || summaries[0].TotalDownloadBytes != 600 || summaries[0].TodayUploadBytes != 300 || summaries[0].TodayDownloadBytes != 400 {
		t.Fatalf("unexpected compacted totals: %+v", summaries)
	}
}

func TestTrafficPeriodsUseBeijingTime(t *testing.T) {
	beforeMidnight := time.Date(2026, 8, 13, 15, 59, 0, 0, time.UTC)
	afterMidnight := time.Date(2026, 8, 13, 16, 1, 0, 0, time.UTC)

	beforeToday, beforeWeek, beforeMonth, beforeQuarter := trafficPeriodStarts(beforeMidnight)
	if got, want := time.Unix(beforeToday, 0), time.Date(2026, 8, 12, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected Beijing day start before midnight: got %s, want %s", got, want)
	}
	if got, want := time.Unix(beforeWeek, 0), time.Date(2026, 8, 9, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected Beijing week start: got %s, want %s", got, want)
	}
	if got, want := time.Unix(beforeMonth, 0), time.Date(2026, 7, 31, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected Beijing month start: got %s, want %s", got, want)
	}
	if got, want := time.Unix(beforeQuarter, 0), time.Date(2026, 6, 30, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected Beijing quarter start: got %s, want %s", got, want)
	}
	afterToday, _, _, _ := trafficPeriodStarts(afterMidnight)
	if got, want := time.Unix(afterToday, 0), time.Date(2026, 8, 13, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("unexpected Beijing day start after midnight: got %s, want %s", got, want)
	}
}

func TestDailyTrafficSplitsAtBeijingMidnight(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 8, 13, 16, 1, 0, 0, time.UTC)
	for _, n := range []domain.Node{{ID: "in", Name: "入口", Role: domain.NodeRoleIngress, CreatedAt: now}, {ID: "out", Name: "出口", Role: domain.NodeRoleEgress, CreatedAt: now}} {
		if err := st.CreateNode(ctx, n, "hash"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SaveRule(ctx, domain.ForwardRule{ID: "rule", Mode: domain.ForwardModeDualManaged, Name: "测试", Protocol: "tcp", IngressNodeID: "in", EgressNodeID: "out", ListenAddress: "0.0.0.0", ListenPort: 10000, RelayPort: 30000, TargetHost: "192.0.2.2", TargetPort: 80, Engine: "nftables", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	deltas := []domain.TrafficDelta{
		{RuleID: "rule", CapturedAt: time.Date(2026, 8, 13, 15, 59, 0, 0, time.UTC), UploadBytes: 100},
		{RuleID: "rule", CapturedAt: time.Date(2026, 8, 13, 16, 1, 0, 0, time.UTC), UploadBytes: 200},
	}
	if err := st.AddTraffic(ctx, "in", deltas); err != nil {
		t.Fatal(err)
	}
	points, err := st.DailyTraffic(ctx, "rule", time.Date(2026, 8, 13, 0, 0, 0, 0, trafficLocation))
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 2 || points[0].UploadBytes != 100 || points[1].UploadBytes != 200 {
		t.Fatalf("expected traffic on both sides of Beijing midnight, got %+v", points)
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
