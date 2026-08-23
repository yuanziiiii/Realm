package main

import (
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
