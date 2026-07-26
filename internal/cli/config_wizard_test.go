package cli

// config_wizard_test.go — 向导状态机测试：直接喂 tea.KeyMsg 驱动 Update（不依赖
// 终端），断言状态迁移与最终落盘的 config。渲染侧抽测关键文案（引导不静默回归）。

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"loop-eng/internal/channel"
	"loop-eng/internal/config"
)

// ---- key 构造助手 ----

func kEnter() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyEnter} }
func kDown() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyDown} }
func kUp() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyUp} }
func kCtrlC() tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyCtrlC} }
func kRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// drive 把一串按键依次喂给 wizard。
func drive(t *testing.T, w *wizard, keys ...tea.KeyMsg) {
	t.Helper()
	for _, k := range keys {
		w.Update(k)
	}
}

// newTestWizard 在 TempDir 脚手架上构造 wizard（与 runConfigWizard 的前置一致）。
func newTestWizard(t *testing.T) (*wizard, string) {
	t.Helper()
	dir := t.TempDir()
	if err := scaffoldLoop(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(dir + "/.loop/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return newWizard(dir, cfg), dir
}

func loadSaved(t *testing.T, dir string) *config.Config {
	t.Helper()
	cfg, err := config.Load(dir + "/.loop/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestWizardGitHubDefaultFlow：欢迎→github→输 repo→默认标签→创建标签→全局→claude→保存。
func TestWizardGitHubDefaultFlow(t *testing.T) {
	stubGHLabelCreate(t, nil)
	w, dir := newTestWizard(t)
	drive(t, w,
		kEnter(),       // welcome → channel
		kDown(),        // github
		kEnter(),       // 选定 → repo 输入
		kRunes("myorg/myrepo"),
		kEnter(),       // repo 确认 → 标签选择
		kEnter(),       // 用默认标签 → 标签创建步骤
		kEnter(),       // 现在就创建 → scope
		kEnter(),       // 全局 → agent
		kEnter(),       // 第一个（claude）→ confirm
		kEnter(),       // 保存 → done
	)
	if !w.saved {
		t.Fatalf("flow should end saved, step=%v quit=%v err=%q", w.step, w.quit, w.errMsg)
	}
	cfg := loadSaved(t, dir)
	if cfg.Channel.Provider != "github" || cfg.Channel.Repo != "myorg/myrepo" || cfg.Channel.TaskLabel != "loop:task" {
		t.Fatalf("channel mismatch: %+v", cfg.Channel)
	}
	for _, role := range []string{"triage", "plan", "execute", "verify"} {
		if got := roleProvider(cfg, role); got != "claude" {
			t.Fatalf("role %s provider = %q, want claude", role, got)
		}
	}
}

// TestWizardGitHubCustomLabel：前缀输入 "ai" → 归一 "ai:"，派生 task_label=ai:task、
// label_prefix=ai: 落盘。
func TestWizardGitHubCustomLabel(t *testing.T) {
	stubGHLabelCreate(t, nil)
	w, dir := newTestWizard(t)
	drive(t, w,
		kEnter(), kDown(), kEnter(),
		kRunes("myorg/myrepo"), kEnter(),
		kDown(), kEnter(), // 自定义标签前缀
		kRunes("ai"), kEnter(),
		kEnter(),                   // 标签创建步骤：现在就创建
		kEnter(), kEnter(), kEnter(), // scope 全局 → claude → 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, err=%q", w.errMsg)
	}
	cfg := loadSaved(t, dir)
	if got := cfg.Channel.TaskLabel; got != "ai:task" {
		t.Fatalf("derived task label = %q, want ai:task", got)
	}
	if got := cfg.Channel.LabelPrefix; got != "ai:" {
		t.Fatalf("label prefix = %q, want ai:", got)
	}
}

// TestWizardRepoRequired：repo 留空（清空预填）回车 → 行内报错且留在原步骤。
func TestWizardRepoRequired(t *testing.T) {
	w, _ := newTestWizard(t)
	drive(t, w, kEnter(), kDown(), kEnter()) // 到 repo 输入（脚手架默认 repo 为空）
	drive(t, w, kEnter())                    // 空输入回车
	if w.step != stepGitHubRepo || w.errMsg == "" {
		t.Fatalf("empty repo must stay with inline error, step=%v err=%q", w.step, w.errMsg)
	}
}

// TestWizardPerRoleAgents：scope=逐角色，plan 选第二个 provider，其余选第一个。
func TestWizardPerRoleAgents(t *testing.T) {
	w, dir := newTestWizard(t)
	drive(t, w,
		kEnter(), kEnter(), // welcome → channel → local
		kEnter(),           // inbox 用预填默认
		kDown(), kEnter(),  // scope=逐角色
	)
	// 4 个角色：triage=第1个, plan=第2个, execute=第1个, verify=第1个
	drive(t, w, kEnter(), kDown(), kEnter(), kEnter(), kEnter())
	drive(t, w, kEnter()) // confirm 保存
	if !w.saved {
		t.Fatalf("should be saved, err=%q", w.errMsg)
	}
	cfg := loadSaved(t, dir)
	names := w.agents
	if got := roleProvider(cfg, "plan"); got != names[1].Value {
		t.Fatalf("plan provider = %q, want %q", got, names[1].Value)
	}
	for _, role := range []string{"triage", "execute", "verify"} {
		if got := roleProvider(cfg, role); got != names[0].Value {
			t.Fatalf("%s provider = %q, want %q", role, got, names[0].Value)
		}
	}
}

// TestWizardCtrlCDiscards：中途 ctrl+c → 不写盘（config.yaml 仍是脚手架默认）。
func TestWizardCtrlCDiscards(t *testing.T) {
	w, dir := newTestWizard(t)
	before, _ := loadSaved(t, dir), error(nil)
	drive(t, w, kEnter(), kDown(), kEnter(), kCtrlC())
	if !w.quit || w.saved {
		t.Fatalf("ctrl+c should quit unsaved, quit=%v saved=%v", w.quit, w.saved)
	}
	after := loadSaved(t, dir)
	if after.Channel.Provider != before.Channel.Provider {
		t.Fatalf("config must be untouched after ctrl+c")
	}
}

// TestWizardGuidanceRenders：渲染抽测——关键引导文案出现在对应步骤的 View 里
// （标题/总开关解释/逐角色含义/确认页摘要/完成页下一步）。
func TestWizardGuidanceRenders(t *testing.T) {
	w, _ := newTestWizard(t)
	if v := w.View(); !strings.Contains(v, "配置向导") || !strings.Contains(v, "任务来源") {
		t.Fatalf("welcome view missing orientation:\n%s", v)
	}
	drive(t, w, kEnter(), kDown(), kEnter(), kRunes("a/b"), kEnter())
	if v := w.View(); !strings.Contains(v, "总开关") || !strings.Contains(v, "preflight") {
		t.Fatalf("label view missing guidance:\n%s", v)
	}
	drive(t, w, kEnter()) // 默认标签 → 标签创建步骤
	if v := w.View(); !strings.Contains(v, "现在就创建") {
		t.Fatalf("labels step view missing create offer:\n%s", v)
	}
	drive(t, w, kDown(), kEnter()) // 跳过创建 → scope 菜单
	if v := w.View(); !strings.Contains(v, "交叉验证") {
		t.Fatalf("scope view missing per-role hint:\n%s", v)
	}
	drive(t, w, kDown(), kEnter()) // 选逐角色 → 第 1 个角色
	if v := w.View(); !strings.Contains(v, "任务分诊") {
		t.Fatalf("per-role view missing role meaning:\n%s", v)
	}
	drive(t, w, kEnter(), kEnter(), kEnter(), kEnter()) // 选完 4 角色 → confirm
	if v := w.View(); !strings.Contains(v, "确认配置") || !strings.Contains(v, "任务来源") {
		t.Fatalf("confirm view missing summary:\n%s", v)
	}
	drive(t, w, kEnter()) // 保存 → done
	if v := w.View(); !strings.Contains(v, "loop-eng doctor") || !strings.Contains(v, "loop-eng daemon") {
		t.Fatalf("done view missing next steps:\n%s", v)
	}
}

// ---- 标签创建步骤（stepGitHubLabels）----

// stubGHLabelCreate 注入假的 gh label create，返回记录与可拨的失败集。
func stubGHLabelCreate(t *testing.T, failOn map[string]bool) *[]string {
	t.Helper()
	old := ghLabelCreate
	var calls []string
	ghLabelCreate = func(repo, name string) error {
		calls = append(calls, name)
		if failOn[name] {
			return errors.New("boom")
		}
		return nil
	}
	t.Cleanup(func() { ghLabelCreate = old })
	return &calls
}

func driveGitHubToLabelsStep(t *testing.T, w *wizard) {
	t.Helper()
	drive(t, w,
		kEnter(), kDown(), kEnter(), // welcome → channel → github
		kRunes("myorg/myrepo"), kEnter(), // repo → 标签选择
		kEnter(), // 用默认标签 → 标签创建步骤
	)
	if w.step != stepGitHubLabels {
		t.Fatalf("should be at labels step, got %v", w.step)
	}
}

// TestWizardLabelsCreateOK：选「现在就创建」→ 任务标签 + 全套 loop:* 状态标签
// 都被创建（与 channel.RequiredGitHubLabels 同一集合），结果说明进 confirm 摘要。
func TestWizardLabelsCreateOK(t *testing.T) {
	calls := stubGHLabelCreate(t, nil)
	w, dir := newTestWizard(t)
	driveGitHubToLabelsStep(t, w)
	drive(t, w, kEnter(), kEnter(), kEnter(), kEnter()) // 创建 → scope 全局 → claude → 保存
	if !w.saved {
		t.Fatalf("should be saved, err=%q", w.errMsg)
	}
	want := channel.RequiredGitHubLabels("", "loop:task")
	if len(*calls) != len(want) {
		t.Fatalf("created %d labels, want %d (%v)", len(*calls), len(want), want)
	}
	seen := map[string]bool{}
	for _, c := range *calls {
		seen[c] = true
	}
	for _, n := range want {
		if !seen[n] {
			t.Fatalf("label %q not created; calls=%v", n, *calls)
		}
	}
	if !strings.Contains(w.labelNote, "✓") {
		t.Fatalf("success note missing, got %q", w.labelNote)
	}
	if v := w.View(); !strings.Contains(v, w.labelNote) {
		t.Fatalf("done view should show the label note")
	}
	_ = dir
}

// TestWizardLabelsCreatePartialFail：部分标签创建失败 → 不阻断流程，结果说明
// 带 ⚠ 与失败名单（preflight 兜底复查）。
func TestWizardLabelsCreatePartialFail(t *testing.T) {
	stubGHLabelCreate(t, map[string]bool{"loop:running": true})
	w, _ := newTestWizard(t)
	driveGitHubToLabelsStep(t, w)
	drive(t, w, kEnter()) // 创建 → scope
	if !strings.Contains(w.labelNote, "⚠") || !strings.Contains(w.labelNote, "loop:running") {
		t.Fatalf("partial-failure note should name the failed label, got %q", w.labelNote)
	}
}

// TestWizardLabelsSkip：选「跳过」→ 零 gh 调用，说明指向 preflight 清单。
func TestWizardLabelsSkip(t *testing.T) {
	calls := stubGHLabelCreate(t, nil)
	w, _ := newTestWizard(t)
	driveGitHubToLabelsStep(t, w)
	drive(t, w, kDown(), kEnter()) // 跳过 → scope
	if len(*calls) != 0 {
		t.Fatalf("skip must not create labels, calls=%v", *calls)
	}
	if !strings.Contains(w.labelNote, "preflight") {
		t.Fatalf("skip note should point at preflight, got %q", w.labelNote)
	}
}

// TestWizardCustomPrefixEmptyFallsBack：误入自定义前缀页，留空回车 = 安全退回
// 默认（label_prefix 空、task_label 用默认）——自定义页不是死胡同。
func TestWizardCustomPrefixEmptyFallsBack(t *testing.T) {
	stubGHLabelCreate(t, nil)
	w, dir := newTestWizard(t)
	drive(t, w,
		kEnter(), kDown(), kEnter(),
		kRunes("myorg/myrepo"), kEnter(),
		kDown(), kEnter(), // 自定义标签前缀
		kEnter(),          // 留空回车 → 默认
		kEnter(),          // 标签创建步骤
		kEnter(), kEnter(), kEnter(), // 全局 → claude → 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, err=%q", w.errMsg)
	}
	cfg := loadSaved(t, dir)
	if cfg.Channel.TaskLabel != "loop:task" || cfg.Channel.LabelPrefix != "" {
		t.Fatalf("empty prefix must fall back to default, got task_label=%q label_prefix=%q",
			cfg.Channel.TaskLabel, cfg.Channel.LabelPrefix)
	}
}

// TestWizardRerunKeepsCustomPrefix：再配置场景——config 已有 label_prefix=ai:，
// 重跑向导选「用默认标签」必须保持 ai: 配对（task_label 保持 ai:task、
// label_prefix 不被抹回 loop:）。钉死审计发现的撕裂 bug。
func TestWizardRerunKeepsCustomPrefix(t *testing.T) {
	stubGHLabelCreate(t, nil)
	w, dir := newTestWizard(t)
	// 预置：已有自定义前缀配置（等价于上一次向导的落盘结果），然后按真实
	// 再配置路径重新构造向导（newWizard 会从 cfg 预填 labelPrefix）。
	w.cfg.Channel.Repo = "myorg/myrepo"
	w.cfg.Channel.TaskLabel = "ai:task"
	w.cfg.Channel.LabelPrefix = "ai:"
	w = newWizard(dir, w.cfg)
	drive(t, w,
		kEnter(), kDown(), kEnter(), // welcome → channel → github
		kEnter(),       // repo 预填 myorg/myrepo → 直接回车
		kEnter(),       // 用默认标签（=保持现状）
		kEnter(),       // 标签创建步骤
		kEnter(), kEnter(), kEnter(), // 全局 → claude → 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, err=%q", w.errMsg)
	}
	cfg := loadSaved(t, dir)
	if cfg.Channel.TaskLabel != "ai:task" || cfg.Channel.LabelPrefix != "ai:" {
		t.Fatalf("rerun must keep the ai: pair, got task_label=%q label_prefix=%q",
			cfg.Channel.TaskLabel, cfg.Channel.LabelPrefix)
	}
}
