package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models    Models    `yaml:"models"`
	Budget    Budget    `yaml:"budget"`
	Verify    Verify    `yaml:"verify"`
	Isolation Isolation `yaml:"isolation"`
	Skills    Skills    `yaml:"skills"`
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

type Isolation struct{ Worktree bool `yaml:"worktree"` }
type Skills struct{ Dir string `yaml:"dir"` }

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
	if c.Models.Triage.Name == "" {
		return fmt.Errorf("models.triage.name 必填")
	}
	return nil
}
