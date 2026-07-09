package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models    Models    `yaml:"models"`
	Budget    Budget    `yaml:"budget"`
	Verify    Verify    `yaml:"verify"`
	Isolation Isolation `yaml:"isolation"`
	Skills    Skills    `yaml:"skills"`
	Channel   Channel   `yaml:"channel"`
	Daemon    Daemon    `yaml:"daemon"`
}

type Channel struct {
	Provider  string `yaml:"provider"`   // "" | "local" | "github"
	Repo      string `yaml:"repo"`       // "owner/name"（github 必填）
	TaskLabel string `yaml:"task_label"` // issue 过滤标签（github 必填）
}

type Models struct {
	Triage  ModelRef `yaml:"triage"`
	Plan    ModelRef `yaml:"plan"`
	Execute ModelRef `yaml:"execute"`
	Verify  ModelRef `yaml:"verify"`
}
type ModelRef struct {
	Provider string   `yaml:"provider"`
	Name     string   `yaml:"name"`
	Via      string   `yaml:"via"`
	Binary   string   `yaml:"binary"`
	Cmd      []string `yaml:"cmd"`
}

type Budget struct {
	PerCallTokens int `yaml:"per_call_tokens"`
	PerTaskTokens int `yaml:"per_task_tokens"`
	MaxRetries    int `yaml:"max_retries"`
}

type Verify struct {
	Deterministic []struct {
		Label string   `yaml:"label"`
		Cmd   []string `yaml:"cmd"`
	} `yaml:"deterministic"`
	Tier3Human bool `yaml:"tier3_human"`
}

type Isolation struct {
	Worktree bool `yaml:"worktree"`
}
type Skills struct {
	Dir string `yaml:"dir"`
}

// Daemon configures the resident engine (spec §8.2). Single-active subloop, no
// concurrency, so no concurrency field. poll_interval <= 0 → daemon command
// falls back to its --poll-interval flag default.
type Daemon struct {
	PollInterval time.Duration `yaml:"poll_interval"`
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.Budget.PerCallTokens <= 0 || c.Budget.PerTaskTokens <= 0 || c.Budget.MaxRetries <= 0 {
		return fmt.Errorf("budget: per_call_tokens/per_task_tokens/max_retries 必须 > 0")
	}
	// 每个 role（triage/plan/execute/verify）必须配置 name 或 binary 之一——
	// 缺任一即硬错误（无法组装对应的 model.Client）。
	for _, r := range []struct {
		role string
		ref  ModelRef
	}{
		{"triage", c.Models.Triage},
		{"plan", c.Models.Plan},
		{"execute", c.Models.Execute},
		{"verify", c.Models.Verify},
	} {
		if r.ref.Name == "" && r.ref.Binary == "" {
			return fmt.Errorf("models.%s 未配置（需 name 或 binary 之一）", r.role)
		}
	}
	if c.Channel.Provider == "github" && (c.Channel.Repo == "" || c.Channel.TaskLabel == "") {
		return fmt.Errorf("channel: github provider 需 repo 与 task_label")
	}
	return nil
}
