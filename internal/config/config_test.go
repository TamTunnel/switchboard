package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 4 {
		t.Fatalf("expected 4 default agents, got %d", len(cfg.Agents))
	}
	if cfg.Agents["claude"].Command[1] != "claude" {
		t.Fatalf("unexpected claude command: %v", cfg.Agents["claude"].Command)
	}
	if cfg.PollIntervalMs != 500 || cfg.DefaultTimeoutS != 600 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	content := `
agents:
  myagent:
    command: ["my-adapter", "run"]
    capabilities: ["code"]
poll_interval_ms: 100
default_timeout_seconds: 60
default_agent: alice
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 || cfg.Agents["myagent"].Command[0] != "my-adapter" {
		t.Fatalf("override failed: %+v", cfg.Agents)
	}
	if cfg.PollIntervalMs != 100 || cfg.DefaultTimeoutS != 60 || cfg.DefaultAgent != "alice" {
		t.Fatalf("scalar overrides failed: %+v", cfg)
	}
}

func TestLoadBadYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	_ = os.WriteFile(p, []byte("agents: [broken"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for malformed YAML")
	}
}
