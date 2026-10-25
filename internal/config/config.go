// Package config loads ~/.switchboard/config.yaml. A missing config is not an
// error: built-in defaults cover the four v1 adapters.
package config

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Agent maps an agent name to the adapter process that runs it.
type Agent struct {
	// Command is the adapter argv, e.g. ["sb-adapter", "claude"].
	Command []string `yaml:"command"`
	// Capabilities are simple tags (e.g. "review", "code") used for cap: routing.
	Capabilities []string `yaml:"capabilities"`
}

// Config is the daemon configuration file.
type Config struct {
	Agents          map[string]Agent `yaml:"agents"`
	PollIntervalMs  int              `yaml:"poll_interval_ms"`
	DefaultTimeoutS int              `yaml:"default_timeout_seconds"`
	DefaultAgent    string           `yaml:"default_agent"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Agents: map[string]Agent{
			"echo":     {Command: []string{"sb-adapter", "echo"}, Capabilities: []string{"echo"}},
			"claude":   {Command: []string{"sb-adapter", "claude"}, Capabilities: []string{"code", "review"}},
			"codex":    {Command: []string{"sb-adapter", "codex"}, Capabilities: []string{"code", "review"}},
			"opencode": {Command: []string{"sb-adapter", "opencode"}, Capabilities: []string{"code", "review"}},
		},
		PollIntervalMs:  500,
		DefaultTimeoutS: 600,
	}
}

// Load reads path, or returns defaults when the file does not exist.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	var file Config
	if err := yaml.Unmarshal(data, &file); err != nil {
		return cfg, err
	}
	if len(file.Agents) > 0 {
		cfg.Agents = file.Agents
	}
	if file.PollIntervalMs > 0 {
		cfg.PollIntervalMs = file.PollIntervalMs
	}
	if file.DefaultTimeoutS > 0 {
		cfg.DefaultTimeoutS = file.DefaultTimeoutS
	}
	if file.DefaultAgent != "" {
		cfg.DefaultAgent = file.DefaultAgent
	}
	return cfg, nil
}

// Path returns the config file path for a switchboard home directory.
func Path(home string) string {
	return filepath.Join(home, "config.yaml")
}
