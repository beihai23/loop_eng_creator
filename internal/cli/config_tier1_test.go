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
