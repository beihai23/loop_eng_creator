package cli

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/config"
	"loop-eng/internal/model"
)

func TestTier1ConfigGhInstallInstructions(t *testing.T) {
	d := ghInstallInstructions("darwin")
	if !strings.Contains(d, "brew install gh") || !strings.Contains(d, "https://cli.github.com/") {
		t.Fatalf("darwin instructions missing brew command or official link: %q", d)
	}
	w := ghInstallInstructions("windows")
	if !strings.Contains(w, "winget install GitHub.cli") || !strings.Contains(w, "https://cli.github.com/") {
		t.Fatalf("windows instructions missing winget command or official link: %q", w)
	}
	l := ghInstallInstructions("linux")
	if !strings.Contains(l, "https://cli.github.com/") || !(strings.Contains(l, "apt") || strings.Contains(l, "curl")) {
		t.Fatalf("linux instructions missing official link or apt/curl command: %q", l)
	}
}

func TestTier1ConfigGhAvailable(t *testing.T) {
	_, err := exec.LookPath("gh")
	if got := ghAvailable(); got != (err == nil) {
		t.Fatalf("ghAvailable()=%v but exec.LookPath(gh) err=%v", got, err)
	}
}

func TestTier1ConfigPickProvider(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1\n", "local"}, {"2\n", "github"}, {"3\n", "linear"},
		{"local\n", "local"}, {"github\n", "github"}, {"linear\n", "linear"},
		{"\n", ""}, {"", ""},
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := pickProvider(bufio.NewReader(strings.NewReader(c.in)), &out)
		if err != nil || got != c.want {
			t.Fatalf("pickProvider(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
		}
	}
}

func TestTier1ConfigSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	mk := func() config.ModelRef { return config.ModelRef{Via: "claude-p", Binary: "claude"} }
	cfg := &config.Config{
		Models: config.Models{Triage: mk(), Plan: mk(), Execute: mk(), Verify: mk()},
		Budget: config.Budget{PerCallTokens: 1, PerTaskTokens: 2, MaxRetries: 3},
		Channel: config.Channel{
			Provider: "linear",
			Linear:   &config.LinearChannel{Project: "proj", Team: "ENG", StatusMap: map[string]string{"started": "started"}},
		},
	}
	if err := config.Save(p, cfg); err != nil {
		t.Fatalf("Save(linear): %v", err)
	}
	got, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load(linear): %v", err)
	}
	if got.Channel.Provider != "linear" || got.Channel.Linear == nil || got.Channel.Linear.Project != "proj" || got.Channel.Linear.Team != "ENG" || got.Channel.Linear.StatusMap["started"] != "started" {
		t.Fatalf("linear round-trip mismatch: %+v", got.Channel)
	}
	cfg.Channel = config.Channel{Provider: "local", Inbox: "todo/"}
	if err := config.Save(p, cfg); err != nil {
		t.Fatalf("Save(local): %v", err)
	}
	got2, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load(local): %v", err)
	}
	if got2.Channel.Provider != "local" || got2.Channel.Inbox != "todo/" {
		t.Fatalf("local round-trip mismatch: %+v", got2.Channel)
	}
}

func TestTier1ConfigInteractiveGithub(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("2\nmyorg/myrepo\n\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("runConfigInteractive(github): %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Channel.Provider != "github" || cfg.Channel.Repo != "myorg/myrepo" || cfg.Channel.TaskLabel != "loop:task" {
		t.Fatalf("github flow mismatch: %+v", cfg.Channel)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loop", "skills")); err != nil {
		t.Fatalf("scaffold missing .loop/skills: %v", err)
	}
}

func TestTier1ConfigInteractiveLinear(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("3\n1\nlin_api_secret_key\nmyproject\n\n\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("runConfigInteractive(linear): %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(raw), "lin_api_secret_key") {
		t.Fatalf("API key leaked into config.yaml")
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Channel.Provider != "linear" || cfg.Channel.Linear == nil || cfg.Channel.Linear.Project != "myproject" {
		t.Fatalf("linear flow mismatch: %+v", cfg.Channel)
	}
	if len(cfg.Channel.Linear.StatusMap) == 0 {
		t.Fatalf("default status_map not written")
	}
	if !strings.Contains(out.String(), "LOOP_ENG_LINEAR_API_KEY") {
		t.Fatalf("env-var instruction not printed, out=%q", out.String())
	}
}

func TestTier1ConfigInteractiveLocal(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("1\nincoming/\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("runConfigInteractive(local): %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Channel.Provider != "local" || cfg.Channel.Inbox != "incoming/" {
		t.Fatalf("local flow mismatch: %+v", cfg.Channel)
	}
}

func TestTier1ConfigInitAlias(t *testing.T) {
	dir := t.TempDir()
	cmd := NewInitCmd()
	cmd.SetIn(strings.NewReader(""))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--repo", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init alias execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loop", "config.yaml")); err != nil {
		t.Fatalf("init alias did not scaffold: %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "config") {
		t.Fatalf("deprecation hint pointing at config not printed; out=%q err=%q", out.String(), errOut.String())
	}
}

// TestTier1ConfigPickAgentProvider exercises the coding-agent provider menu in
// isolation: number and name both select (case-insensitively, trimmed); empty
// line and EOF both yield ("", nil) = keep current; an invalid line reprints the
// menu and re-reads. The expected values are derived from the live registry
// (model.RegisteredProviders), so the test stays valid as providers are added.
func TestTier1ConfigPickAgentProvider(t *testing.T) {
	names := model.RegisteredProviders()
	cases := []struct{ in, want string }{
		{"1\n", names[0]},                            // 1-based number → first registered
		{"2\n", names[1]},                            // number → second registered
		{names[0] + "\n", names[0]},                  // canonical name
		{strings.ToUpper(names[0]) + "\n", names[0]}, // name, case-insensitive
		{" " + names[0] + " \n", names[0]},           // trimmed
		{"\n", ""},                                   // empty line → keep
		{"", ""},                                     // EOF → keep
		{"bogus\n" + names[0] + "\n", names[0]},      // invalid → reprint → valid
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := pickAgentProvider(bufio.NewReader(strings.NewReader(c.in)), &out)
		if err != nil || got != c.want {
			t.Fatalf("pickAgentProvider(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
		}
	}
}

// TestTier1ConfigInteractiveAgentProviderGlobal: local channel + scope=1 (全局
// 一把梭) + 选 codex → 四角色 models.<role>.provider 皆 codex。选 codex 用 name
// 输入（不依赖 registry 排序，新增 provider 也不会让本用例漂移）。
func TestTier1ConfigInteractiveAgentProviderGlobal(t *testing.T) {
	dir := t.TempDir()
	// local channel, inbox=inbox/, scope=1 global, pick codex by name
	in := strings.NewReader("1\ninbox/\n1\ncodex\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("global agent-provider flow: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	for _, role := range []string{"triage", "plan", "execute", "verify"} {
		if got := roleProvider(cfg, role); got != "codex" {
			t.Fatalf("role %s provider = %q, want codex (global); out=%q", role, got, out.String())
		}
	}
}

// TestTier1ConfigInteractiveAgentProviderPerRole: scope=2 逐角色——plan 选 codex，
// 其余角色空输入保持 scaffold 默认 claude。
func TestTier1ConfigInteractiveAgentProviderPerRole(t *testing.T) {
	dir := t.TempDir()
	// local, inbox, scope=2 逐角色; triage 空(keep), plan codex, execute 空(keep), verify 空(keep)
	in := strings.NewReader("1\ninbox/\n2\n\ncodex\n\n\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("per-role agent-provider flow: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	want := map[string]string{"triage": "claude", "plan": "codex", "execute": "claude", "verify": "claude"}
	for role, w := range want {
		if got := roleProvider(cfg, role); got != w {
			t.Fatalf("role %s provider = %q, want %q; out=%q", role, got, w, out.String())
		}
	}
}

// TestTier1ConfigInteractiveAgentProviderEmptyKeepsDefault: channel 选了 local
// 但 scope 行 EOF/空 → 不改 provider，四角色仍是 scaffold 默认 claude。这保证现有
// piped-stdin 脚本（channel 步用尽 stdin 后尾部 EOF）零回归。
func TestTier1ConfigInteractiveAgentProviderEmptyKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	// local, inbox=inbox/, then EOF at the scope prompt
	in := strings.NewReader("1\ninbox/\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("empty-scope flow: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	for _, role := range []string{"triage", "plan", "execute", "verify"} {
		if got := roleProvider(cfg, role); got != "claude" {
			t.Fatalf("role %s provider = %q, want claude (kept default)", role, got)
		}
	}
}

// TestTier1ConfigScaffoldHasProviderClaude: 空 stdin → 脚手架后 pickProvider EOF
// → 保持默认。scaffold 写入的 config.yaml 每个角色都含 provider: claude（开箱
// claude、行为与 #68 前一致）。
func TestTier1ConfigScaffoldHasProviderClaude(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runConfigInteractive(strings.NewReader(""), &out, dir); err != nil {
		t.Fatalf("scaffold flow: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("read scaffold config: %v", err)
	}
	if cnt := strings.Count(string(raw), "provider: claude"); cnt != 4 {
		t.Fatalf("scaffold config.yaml should carry provider: claude on all 4 roles, got %d:\n%s", cnt, raw)
	}
}
