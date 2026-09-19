package cli

// M1 出题权分离（test-prep）在 CLI 装配层的测试：
//   - buildModels 按 models.test_prep 是否配置返回/省略 test-prep skill（fake 模式）。
//   - embed 的 test-prep.md 模板可渲染（语法/字段齐备），白名单字段与条件块都渲染。
//   - plan.md 的 ExamSeparate 条件化：启用时出题职责段消失，legacy 时保留。

import (
	"strings"
	"testing"

	"loop-eng/internal/budget"
	"loop-eng/internal/config"
	"loop-eng/internal/skill"
)

// driveToTestPrepOpt 把向导驱动到 test-prep 取舍步（welcome → local → inbox →
// scope 全局 → claude）。
func driveToTestPrepOpt(t *testing.T, w *wizard) {
	t.Helper()
	drive(t, w,
		kEnter(), kEnter(), // welcome → channel → local
		kEnter(), // inbox 用预填默认
		kEnter(), // scope 全局
		kEnter(), // claude → test-prep 取舍
	)
}

// TestWizardTestPrepOptIn：取舍步选「启用」→ 选引擎 → 保存后 config 落
// test_prep（provider/binary=所选引擎，readonly 强制 true——出题人只读）。
func TestWizardTestPrepOptIn(t *testing.T) {
	w, dir := newTestWizard(t)
	driveToTestPrepOpt(t, w)
	drive(t, w,
		kDown(),  // 启用出题权分离
		kEnter(), // 选定 → 选引擎
		kEnter(), // 第一个引擎 → confirm
		kEnter(), // 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, step=%v err=%q", w.step, w.errMsg)
	}
	tp := loadSaved(t, dir).Models.TestPrep
	want := w.agents[0].Value
	if tp.IsZero() || tp.Provider != want || tp.Binary != want || !tp.ReadOnly {
		t.Fatalf("test_prep 未按启用写入: %+v (want provider/binary=%s readonly=true)", tp, want)
	}
}

// TestWizardTestPrepRerunKeepsEnabled：再配置场景——已启用的 config 重跑向导，
// 取舍步光标预置在「启用」上，回车即维持现状，不清零。
func TestWizardTestPrepRerunKeepsEnabled(t *testing.T) {
	w, dir := newTestWizard(t)
	w.cfg.Models.TestPrep = config.ModelRef{Provider: "codex", Binary: "codex", ReadOnly: true}
	w = newWizard(dir, w.cfg)
	driveToTestPrepOpt(t, w)
	if w.cursor != 1 {
		t.Fatalf("已启用时取舍步光标应预置在「启用」(1), got %d", w.cursor)
	}
	drive(t, w,
		kEnter(), // 维持启用 → 选引擎
		kEnter(), // 第一个引擎 → confirm
		kEnter(), // 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, step=%v err=%q", w.step, w.errMsg)
	}
	if loadSaved(t, dir).Models.TestPrep.IsZero() {
		t.Fatal("再配置维持启用不应清零 test_prep")
	}
}

// TestWizardTestPrepDeclineClears：已启用的 config 显式选「不启用」→ 保存后
// test_prep 清零（回 legacy）。否则再配置抹不掉旧配置。
func TestWizardTestPrepDeclineClears(t *testing.T) {
	w, dir := newTestWizard(t)
	w.cfg.Models.TestPrep = config.ModelRef{Provider: "codex", Binary: "codex", ReadOnly: true}
	w = newWizard(dir, w.cfg)
	driveToTestPrepOpt(t, w)
	drive(t, w,
		kUp(),    // 移回「不启用」
		kEnter(), // 不启用 → confirm（跳过选引擎）
		kEnter(), // 保存
	)
	if !w.saved {
		t.Fatalf("should be saved, step=%v err=%q", w.step, w.errMsg)
	}
	if !loadSaved(t, dir).Models.TestPrep.IsZero() {
		t.Fatal("显式不启用应清零 test_prep")
	}
}

func TestBuildModelsTestPrepOptional(t *testing.T) {
	mk := func(tp config.ModelRef) *config.Config {
		return &config.Config{
			Models: config.Models{
				Triage:   config.ModelRef{Binary: "claude"},
				Plan:     config.ModelRef{Binary: "claude"},
				Execute:  config.ModelRef{Binary: "claude"},
				Verify:   config.ModelRef{Binary: "claude"},
				TestPrep: tp,
			},
		}
	}
	if _, _, _, _, _, tp := buildModels(mk(config.ModelRef{}), "fake", budget.New(1000, 10000, 1)); tp != nil {
		t.Fatalf("未配置 test_prep 时 buildModels 应返回 nil（legacy）, got %+v", tp)
	}
	_, _, _, _, _, tp := buildModels(mk(config.ModelRef{Binary: "claude"}), "fake", budget.New(1000, 10000, 1))
	if tp == nil {
		t.Fatal("配置了 test_prep 时 buildModels 应返回 test-prep skill")
	}
	if tp.Name != "test-prep" {
		t.Fatalf("test-prep skill name 错误: %q", tp.Name)
	}
}

// TestTestPrepEmbedRenders：embed 的 test-prep.md 是 text/template——语法错误/
// 字段名笔误在渲染时暴露；白名单字段与「默认沿用」条件块必须真实渲染。
func TestTestPrepEmbedRenders(t *testing.T) {
	_, _, _, _, _, tp := buildModels(&config.Config{
		Models: config.Models{
			Triage:   config.ModelRef{Binary: "claude"},
			Plan:     config.ModelRef{Binary: "claude"},
			Execute:  config.ModelRef{Binary: "claude"},
			Verify:   config.ModelRef{Binary: "claude"},
			TestPrep: config.ModelRef{Binary: "claude"},
		},
	}, "fake", budget.New(1000, 10000, 1))
	if tp == nil {
		t.Fatal("test-prep skill 未装配")
	}
	prompt := renderTemplate(t, tp.PromptTmpl, skill.TestPrepInput{
		Task:               "加登录限流",
		AcceptanceCriteria: []string{"标准甲"},
		Body:               "背景正文",
		PriorExam:          `{"criteria":["旧考卷"]}`,
	})
	for _, want := range []string{"独立出题人", "标准甲", "背景正文", "上一轮考卷", "旧考卷", "禁止发明"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("test-prep prompt 缺少 %q", want)
		}
	}
}

// TestPlanEmbedExamSeparate：plan.md 的 ExamSeparate 条件化——启用时验收标准评审
// 与 verify_script 职责段不渲染（plan 只规划实施），legacy 时保留（渲染与旧版一致）。
func TestPlanEmbedExamSeparate(t *testing.T) {
	_, plan, _, _, _, _ := buildModels(&config.Config{
		Models: config.Models{
			Triage:  config.ModelRef{Binary: "claude"},
			Plan:    config.ModelRef{Binary: "claude"},
			Execute: config.ModelRef{Binary: "claude"},
			Verify:  config.ModelRef{Binary: "claude"},
		},
	}, "fake", budget.New(1000, 10000, 1))

	in := skill.PlanInput{Task: "t", AcceptanceCriteria: []string{"c"}, ExamSeparate: true}
	sep := renderTemplate(t, plan.PromptTmpl, in)
	// 负向断言打在段落标题级（启用分支里「不要输出 revised_criteria」的告诫文字
	// 是有意保留的，不算职责段）。
	if strings.Contains(sep, "### 验收标准评审") || strings.Contains(sep, "### verify_script") {
		t.Fatalf("ExamSeparate=true 时不应渲染出题职责段:\n%s", sep)
	}
	if !strings.Contains(sep, "职责边界") || !strings.Contains(sep, "test-prep") {
		t.Fatalf("ExamSeparate=true 时应渲染职责边界说明:\n%s", sep)
	}

	in.ExamSeparate = false
	legacy := renderTemplate(t, plan.PromptTmpl, in)
	for _, want := range []string{"验收标准评审", "revised_criteria", "verify_script"} {
		if !strings.Contains(legacy, want) {
			t.Fatalf("legacy 渲染应保留出题职责段 %q", want)
		}
	}
}
