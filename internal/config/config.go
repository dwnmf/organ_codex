package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Model                string                    `toml:"model"`
	ModelProvider        string                    `toml:"model_provider"`
	ModelReasoningEffort string                    `toml:"model_reasoning_effort"`
	ApprovalPolicy       string                    `toml:"approval_policy"`
	SandboxMode          string                    `toml:"sandbox_mode"`
	Personality          string                    `toml:"personality"`
	WebSearch            string                    `toml:"web_search"`
	Features             map[string]bool           `toml:"features"`
	MCPServers           map[string]MCPServer      `toml:"mcp_servers"`
	ModelProviders       map[string]ModelProvider  `toml:"model_providers"`
	Orchestrator         OrchestratorRuntimeConfig `toml:"orchestrator"`
	Raw                  map[string]any            `toml:"-"`
	Path                 string                    `toml:"-"`
}

type MCPServer struct {
	URL string `toml:"url"`
}

type ModelProvider struct {
	Name               string `toml:"name"`
	BaseURL            string `toml:"base_url"`
	WireAPI            string `toml:"wire_api"`
	ChatGPTBaseURL     string `toml:"chatgpt_base_url"`
	RequiresOpenAIAuth bool   `toml:"requires_openai_auth"`
}

type OrchestratorRuntimeConfig struct {
	Addr               string `toml:"addr"`
	DBPath             string `toml:"db_path"`
	WorkspaceRoot      string `toml:"workspace_root"`
	DispatchIntervalMS int    `toml:"dispatch_interval_ms"`
	WatchdogIntervalMS int    `toml:"watchdog_interval_ms"`
	RetryDelayMS       int    `toml:"retry_delay_ms"`
	MaxRetries         int    `toml:"max_retries"`
	IdleTimeoutMS      int    `toml:"idle_timeout_ms"`
	DefaultMaxHops     int    `toml:"default_max_hops"`
}

func Load(path string) (Config, error) {
	resolved := path
	if resolved == "" {
		resolved = defaultConfigPath()
	}
	if strings.HasPrefix(resolved, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve home directory: %w", err)
		}
		trimmed := strings.TrimPrefix(resolved, "~")
		trimmed = strings.TrimPrefix(trimmed, "\\")
		trimmed = strings.TrimPrefix(trimmed, "/")
		resolved = filepath.Join(home, trimmed)
	}
	resolved = filepath.Clean(resolved)

	bytes, err := os.ReadFile(resolved)
	if err != nil {
		return Config{}, fmt.Errorf("read config file %s: %w", resolved, err)
	}

	var cfg Config
	if _, err := toml.Decode(string(bytes), &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config file: %w", err)
	}
	var raw map[string]any
	if _, err := toml.Decode(string(bytes), &raw); err != nil {
		return Config{}, fmt.Errorf("decode raw config: %w", err)
	}
	cfg.Raw = raw
	cfg.Path = resolved
	return cfg, nil
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex/config.toml"
	}
	return filepath.Join(home, ".codex", "config.toml")
}
