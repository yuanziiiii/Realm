package main

import (
	"testing"
	"time"
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
