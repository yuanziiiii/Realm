package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"relaypanel/internal/domain"
)

func TestTargetProbeDueAtConfiguredInterval(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if !targetProbeDue(time.Time{}, 60*time.Second, now) {
		t.Fatal("an agent without probe history must probe immediately")
	}
	if targetProbeDue(now.Add(-59*time.Second), 60*time.Second, now) {
		t.Fatal("target probe ran before the configured interval")
	}
	if !targetProbeDue(now.Add(-60*time.Second), 60*time.Second, now) {
		t.Fatal("target probe did not run when the configured interval elapsed")
	}
}

func TestAppliedConfigurationMatchesRevisionAndFingerprint(t *testing.T) {
	st := &state{AppliedRevision: 12, AppliedConfigHash: "old"}
	if appliedConfigurationMatches(domain.SyncResponse{Revision: 13, ConfigHash: "old"}, st) {
		t.Fatal("a newer revision was treated as already applied")
	}
	if appliedConfigurationMatches(domain.SyncResponse{Revision: 12, ConfigHash: "new"}, st) {
		t.Fatal("changed configuration content was ignored at the same revision")
	}
	if !appliedConfigurationMatches(domain.SyncResponse{Revision: 12, ConfigHash: "old"}, st) {
		t.Fatal("matching revision and configuration fingerprint was not recognized")
	}
	if !appliedConfigurationMatches(domain.SyncResponse{Revision: 12}, st) {
		t.Fatal("an older controller without fingerprints lost compatibility")
	}
	if appliedConfigurationMatches(domain.SyncResponse{Revision: 12, ConfigHash: "new"}, &state{AppliedRevision: 12}) {
		t.Fatal("an upgraded agent skipped the first fingerprinted configuration")
	}
}

func TestSummarizeAccessRulesKeepsReadablePolicyWithoutExpandedRanges(t *testing.T) {
	policy := domain.AccessPolicy{
		Enabled: true, AllowCIDRs: []string{"203.0.113.8"}, DenyCIDRs: []string{"198.51.100.0/24"},
		AllowRegions: []string{"广东省/广州市"}, DenyRegions: []string{"浙江省/杭州市"},
		ResolvedAllowRanges: []string{"11.0.0.0/8"}, ResolvedDenyRanges: []string{"12.0.0.0/8"},
		MaxTCPConnectionsPerIP: 8,
	}
	deployments := []domain.Deployment{
		{Rule: domain.ForwardRule{ID: "rule_demo", Name: "游戏", ListenPort: 31259, Protocol: "both", AccessPolicy: policy}, Role: domain.NodeRoleIngress},
		{Rule: domain.ForwardRule{ID: "rule_exit", Name: "非客户端出口", AccessPolicy: policy}, Role: domain.NodeRoleEgress},
	}
	got := summarizeAccessRules(deployments)
	if len(got) != 1 {
		t.Fatalf("expected one client-facing access summary, got %#v", got)
	}
	if got[0].RuleID != "rule_demo" || got[0].AllowRegions[0] != "广东省/广州市" || got[0].MaxTCPConnectionsPerIP != 8 {
		t.Fatalf("unexpected access summary: %#v", got[0])
	}
	encoded, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "11.0.0.0/8") || strings.Contains(string(encoded), "12.0.0.0/8") {
		t.Fatalf("expanded ranges leaked into the readable Agent state: %s", encoded)
	}
}
