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
	Provider  string `yaml:"provider"`   // "" | "local" | "github" | "linear"
	Repo      string `yaml:"repo"`       // "owner/name"（github 必填）
	TaskLabel string `yaml:"task_label"` // issue 过滤标签（github 必填）
	// LabelPrefix 是 loop:<status> 状态标签族的前缀（github），空 = "loop:"。
	// 多实例共存同一仓库或组织命名规范时自定义（如 "ai:" → ai:running/ai:done…）。
	// 与 TaskLabel 独立：身份标签可以单独是任何名字，状态族只看前缀。
	LabelPrefix string `yaml:"label_prefix,omitempty"`
	// Inbox 是 local provider 的 inbox 路径（相对 repo 根），空 = 默认 "inbox"。
	Inbox string `yaml:"inbox,omitempty"`
	// Linear 是 linear provider 的配置块；指针为 nil 时整块省略。
	// API key 不进 config.yaml（#24 决定 A）——走 env LOOP_ENG_LINEAR_API_KEY
	// 或 gitignore 的 .loop/linear.key。
	Linear *LinearChannel `yaml:"linear,omitempty"`
}

// LinearChannel carries the linear provider config (see
// docs/superpowers/specs/linear-channel-mapping.md §9).
type LinearChannel struct {
	Project   string            `yaml:"project"`              // name 或 uuid（按 project 过滤，linear 必填）
	Team      string            `yaml:"team,omitempty"`       // team key（如 ENG），可空
	StatusMap map[string]string `yaml:"status_map,omitempty"` // loop status / state type → Linear WorkflowState
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

// Verify carries the verify-chain config. There is NO static tier-1 script list
// here — tier-1 acceptance scripts are produced per-task by the planner
// (skill.PlanOutput.VerifyScript) and run in the worktree. config only carries
// the tier-3 human-review switch.
type Verify struct {
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
	if c.Channel.Provider == "linear" && (c.Channel.Linear == nil || c.Channel.Linear.Project == "") {
		return fmt.Errorf("channel: linear provider 需 linear.project")
	}
	return nil
}

// Save writes c as YAML to path (0644), replacing the file wholesale. It does
// NOT validate — Load is the validation gate (a half-edited config can still
// be written; it just won't load). Save/Load round-trip is guaranteed for
// every field, including daemon.poll_interval (time.Duration: 60s ↔ 1m0s).
func Save(path string, c *Config) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, raw, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
