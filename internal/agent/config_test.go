package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaultsTargetProbeIntervalToSixtySeconds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"controller_url":"https://panel.example","node_id":"node","token":"token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetProbeInterval != 60*time.Second || cfg.TargetProbeIntervalText != "60s" {
		t.Fatalf("unexpected target probe interval: duration=%s text=%q", cfg.TargetProbeInterval, cfg.TargetProbeIntervalText)
	}
}

func TestLoadConfigAcceptsExplicitTargetProbeInterval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"controller_url":"https://panel.example","node_id":"node","token":"token","target_probe_interval":"90s"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TargetProbeInterval != 90*time.Second {
		t.Fatalf("unexpected target probe interval: %s", cfg.TargetProbeInterval)
	}
}
