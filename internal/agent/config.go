package agent

import (
	"encoding/json"
	"os"
	"time"
)

type Config struct {
	ControllerURL     string        `json:"controller_url"`
	NodeID            string        `json:"node_id"`
	Token             string        `json:"token"`
	Apply             bool          `json:"apply"`
	AllowQdiscReplace bool          `json:"allow_qdisc_replace"`
	SyncInterval      time.Duration `json:"-"`
	SyncIntervalText  string        `json:"sync_interval"`
	StateDir          string        `json:"state_dir"`
	RealmBinary       string        `json:"realm_binary"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.SyncIntervalText == "" {
		c.SyncIntervalText = "10s"
	}
	c.SyncInterval, err = time.ParseDuration(c.SyncIntervalText)
	if err != nil {
		return c, err
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/relay-agent"
	}
	if c.RealmBinary == "" {
		c.RealmBinary = "realm"
	}
	return c, nil
}
