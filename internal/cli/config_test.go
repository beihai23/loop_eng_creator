// internal/cli/config_test.go
package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/channel"
	"loop-eng/internal/config"
)

func TestGhInstallInstructionsPlatforms(t *testing.T) {
	d := ghInstallInstructions("darwin")
	if !strings.Contains(d, "brew install gh") || !strings.Contains(d, "https://cli.github.com/") {
		t.Fatalf("darwin instructions incomplete: %q", d)
	}
	w := ghInstallInstructions("windows")
	if !strings.Contains(w, "winget install GitHub.cli") || !strings.Contains(w, "https://cli.github.com/") {
		t.Fatalf("windows instructions incomplete: %q", w)
	}
	l := ghInstallInstructions("linux")
	if !strings.Contains(l, "https://cli.github.com/") || !(strings.Contains(l, "apt") || strings.Contains(l, "curl")) {
		t.Fatalf("linux instructions incomplete: %q", l)
	}
	other := ghInstallInstructions("plan9")
	if !strings.Contains(other, "https://cli.github.com/") {
		t.Fatalf("unknown goos should still carry the official link: %q", other)
	}
}

func TestPickProviderInputs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1\n", "local"}, {"2\n", "github"}, {"3\n", "linear"},
		{"LOCAL\n", "local"}, {"GitHub\n", "github"}, {" linear \n", "linear"},
		{"\n", ""}, {"", ""},
		{"bogus\n2\n", "github"}, // 非法输入重印菜单重读
	}
	for _, c := range cases {
		var out bytes.Buffer
		got, err := pickProvider(bufio.NewReader(strings.NewReader(c.in)), &out)
		if err != nil || got != c.want {
			t.Fatalf("pickProvider(%q) = %q, %v; want %q, nil", c.in, got, err, c.want)
		}
	}
}

func TestPromptLineDefaultAndEOF(t *testing.T) {
	var out bytes.Buffer
	// 空行 → 默认值
	got, err := promptLine(bufio.NewReader(strings.NewReader("\n")), &out, "field", "def")
	if err != nil || got != "def" {
		t.Fatalf("empty line = %q, %v; want def, nil", got, err)
	}
	// EOF → 默认值
	got, err = promptLine(bufio.NewReader(strings.NewReader("")), &out, "field", "def")
	if err != nil || got != "def" {
		t.Fatalf("EOF = %q, %v; want def, nil", got, err)
	}
	// 非空输入（无换行 + EOF）→ 用输入
	got, err = promptLine(bufio.NewReader(strings.NewReader("  value ")), &out, "field", "def")
	if err != nil || got != "value" {
		t.Fatalf("partial line = %q, %v; want value, nil", got, err)
	}
	// prompt 文案带默认值
	out.Reset()
	if _, err := promptLine(bufio.NewReader(strings.NewReader("\n")), &out, "repo", "a/b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "repo [a/b]: ") {
		t.Fatalf("prompt missing default hint: %q", out.String())
	}
}

func TestParseStatusMap(t *testing.T) {
	if got := parseStatusMap(""); len(got) != 0 {
		t.Fatalf("empty input should yield empty map, got %v", got)
	}
	got := parseStatusMap("running=In Progress, done=Done,,bogus")
	if got["running"] != "In Progress" || got["done"] != "Done" || len(got) != 2 {
		t.Fatalf("parse mismatch: %v", got)
	}
}

func TestDefaultLinearStatusMap(t *testing.T) {
	m := defaultLinearStatusMap()
	for _, typ := range []string{"backlog", "unstarted", "started", "completed", "canceled"} {
		if m[typ] != typ {
			t.Fatalf("identity fallback missing %q: %v", typ, m)
		}
	}
}

func TestConfigCmdGithubFlow(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("2\nmyorg/myrepo\n\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("github flow: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Channel.Provider != "github" || cfg.Channel.Repo != "myorg/myrepo" || cfg.Channel.TaskLabel != "loop:task" {
		t.Fatalf("github flow mismatch: %+v", cfg.Channel)
	}
}

// TestConfigCmdGithubFlowGuidance 钉死「小白向引导文案不回归」：github 流程的输出
// 必须带开场定向、task_label 的用途解释（「总开关」）、以及收尾的下一步指引——
// 这些是 #UX 重设计的核心交付，静默丢失（如 refactor 删了 print）要能红。
func TestConfigCmdGithubFlowGuidance(t *testing.T) {
	dir := t.TempDir()
	in := strings.NewReader("2\nmyorg/myrepo\n\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("github flow: %v", err)
	}
	for _, want := range []string{
		"配置向导",       // 开场定向
		"任务来源",       // channel 是什么
		"总开关",         // task_label 的用途解释（评测点的核心缺失）
		"preflight",      // 状态标签谁来检查
		"干活引擎",       // coding-agent 是什么
		"loop-eng doctor", // 收尾下一步
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("guidance text missing %q; out=\n%s", want, out.String())
		}
	}
}

func TestConfigCmdLinearFlowKeyNotInConfig(t *testing.T) {
	dir := t.TempDir()
	// 选 2：写 .loop/linear.key
	in := strings.NewReader("3\n2\nlin_api_secret_key\nmyproject\nENG\nrunning=In Progress,done=Done\n")
	var out bytes.Buffer
	if err := runConfigInteractive(in, &out, dir); err != nil {
		t.Fatalf("linear flow: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lin_api_secret_key") {
		t.Fatalf("API key leaked into config.yaml:\n%s", raw)
	}
	keyRaw, err := os.ReadFile(filepath.Join(dir, ".loop", "linear.key"))
	if err != nil {
		t.Fatalf("linear.key not written: %v", err)
	}
	if strings.TrimSpace(string(keyRaw)) != "lin_api_secret_key" {
		t.Fatalf("linear.key mismatch: %q", keyRaw)
	}
	fi, _ := os.Stat(filepath.Join(dir, ".loop", "linear.key"))
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("linear.key perm = %v, want 0600", fi.Mode().Perm())
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatalf("load written config: %v", err)
	}
	if cfg.Channel.Provider != "linear" || cfg.Channel.Linear == nil ||
		cfg.Channel.Linear.Project != "myproject" || cfg.Channel.Linear.Team != "ENG" ||
		cfg.Channel.Linear.StatusMap["running"] != "In Progress" {
		t.Fatalf("linear flow mismatch: %+v", cfg.Channel)
	}
}

func TestConfigCmdLocalFlow(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runConfigInteractive(strings.NewReader("1\nincoming/\n"), &out, dir); err != nil {
		t.Fatalf("local flow: %v", err)
	}
	cfg, err := config.Load(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channel.Provider != "local" || cfg.Channel.Inbox != "incoming/" {
		t.Fatalf("local flow mismatch: %+v", cfg.Channel)
	}
}

func TestConfigCmdEmptyInputKeepsConfig(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	// EOF（脚手架后）→ 保持默认配置、不报错、不 Save（仍是脚手架的默认文件）。
	if err := runConfigInteractive(strings.NewReader(""), &out, dir); err != nil {
		t.Fatalf("EOF flow: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".loop", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != defaultConfig {
		t.Fatalf("config.yaml should remain the scaffold default, got:\n%s", raw)
	}
	if !strings.Contains(out.String(), "保持当前配置") {
		t.Fatalf("expected keep-current notice, out=%q", out.String())
	}
}

func TestInitAliasDeprecationAndScaffold(t *testing.T) {
	dir := t.TempDir()
	cmd := NewInitCmd()
	cmd.SetIn(strings.NewReader(""))
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--repo", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init alias: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".loop", "config.yaml")); err != nil {
		t.Fatalf("init alias did not scaffold: %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "config") {
		t.Fatalf("deprecation hint missing; out=%q err=%q", out.String(), errOut.String())
	}
}

func TestBuildChannelLocalInboxAndLinearError(t *testing.T) {
	cfg := &config.Config{Channel: config.Channel{Provider: "local", Inbox: "todo/"}}
	ch, err := buildChannel(cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l, ok := ch.(*channel.Local)
	if !ok {
		t.Fatalf("local provider should build *channel.Local, got %T", ch)
	}
	if l.InboxDir != "todo/" {
		t.Fatalf("InboxDir = %q, want todo/", l.InboxDir)
	}
	cfg2 := &config.Config{Channel: config.Channel{Provider: "linear"}}
	if _, err := buildChannel(cfg2, t.TempDir()); err == nil || !strings.Contains(err.Error(), "linear") {
		t.Fatalf("linear provider should yield explicit not-implemented error, got %v", err)
	}
}
