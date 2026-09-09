package cli

// accept_land_test.go —— loop:accept 拦截（tier-3 人审接受→落地）+ humanTierFor
// 按通道能力接线的验收测试。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loop-eng/internal/channel"
	"loop-eng/internal/config"
	"loop-eng/internal/state"
	"loop-eng/internal/verify"
)

// parkedTaskFixture 建好 accept 的前置：git 仓库（含 park 分支上的一个提交）+
// state 库（needs-review 任务 + land_branch + accept 反馈）。返回 (repo, st, taskID, root)。
func parkedTaskFixture(t *testing.T, feedback string) (string, *state.Store, string, string) {
	t.Helper()
	repo := t.TempDir()
	initGitRepo(t, repo)
	wt := filepath.Join(repo, ".loop", "worktrees", "task-p1-r1")
	addWorktreeWithCommit(t, repo, wt, "loop/task-p1-r1")

	st, err := state.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := st.InsertTask(state.TaskRow{IssueRef: "9", Description: "d", Title: "T"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLandBranch(id, "loop/task-p1-r1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetResumeFeedback(id, feedback); err != nil {
		t.Fatal(err)
	}
	return repo, st, id, t.TempDir()
}

// localCfg 是 local provider 的最小配置（无 GitHub remote → createPR 走
// errNoGitHubRemote → finalizeLand 本地 FF-merge 兜底 → Integrated）。
func localCfg() *config.Config {
	return &config.Config{Channel: config.Channel{Provider: "local"}}
}

// TestAcceptParkedReviewLandsBranch 核验接受语义：含 loop:accept 的人回复 +
// park 分支在案 → 不重跑 loop，直接 FF-merge 落地、关单、发 DONE 评论、消费反馈。
// 「落地人审过的提交、而非重跑产出新代码」是 accept 的正确性核心。
func TestAcceptParkedReviewLandsBranch(t *testing.T) {
	repo, st, id, root := parkedTaskFixture(t, "看过 diff，loop:accept 谢谢")
	ch := channel.NewLocal(root)
	task, err := st.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	detail, ok := acceptParkedReview(context.Background(), st, ch, repo, localCfg(), task)
	if !ok {
		t.Fatalf("含令牌 + 有分支的 accept 必须被处理，detail=%q", detail)
	}
	// 落地：park 分支的提交 FF-merge 进当前分支（new.go 出现在仓库根）。
	if _, err := os.Stat(filepath.Join(repo, "new.go")); err != nil {
		t.Fatalf("accept 后 park 分支必须已 FF-merge（new.go 缺失）: %v", err)
	}
	// 反馈已消费：不会在重跑路径再次弹出。
	if fb, err := st.PopResumeFeedback(id); err != nil || fb != "" {
		t.Fatalf("accept 后反馈应被消费，got %q err=%v", fb, err)
	}
	// DONE 评论落到 outbox（人可见的接受回执）。
	body, rerr := os.ReadFile(filepath.Join(root, "outbox", "9.md"))
	if rerr != nil || !strings.Contains(string(body), "DONE: tier-3 人审接受") {
		t.Fatalf("outbox 应有 DONE 接受回执（err=%v）", rerr)
	}
}

// TestAcceptParkedReviewNoTokenNotHandled 核验驳回语义：反馈无令牌 → 不拦截，
// 照常走 SubLoop.Run 带反馈重跑（reject/反馈路径不受影响）。
func TestAcceptParkedReviewNoTokenNotHandled(t *testing.T) {
	repo, st, id, root := parkedTaskFixture(t, "变量名改成 foo")
	ch := channel.NewLocal(root)
	task, _ := st.GetTask(id)
	if _, ok := acceptParkedReview(context.Background(), st, ch, repo, localCfg(), task); ok {
		t.Fatal("无令牌的反馈不得触发 accept 拦截（那是驳回重跑路径）")
	}
	// 反馈未被消费（留给 SubLoop.Run 弹出）。
	if fb, err := st.GetResumeFeedback(id); err != nil || fb == "" {
		t.Fatalf("无令牌时反馈必须保留给重跑路径，got %q err=%v", fb, err)
	}
}

// TestAcceptParkedReviewNoBranchDegrades 核验降级：有令牌但无分支记录（park 时
// commit 失败的旧 park）→ 不拦截、不崩，降级为重跑路径。
func TestAcceptParkedReviewNoBranchDegrades(t *testing.T) {
	repo, st, id, root := parkedTaskFixture(t, "loop:accept")
	if err := st.SetLandBranch(id, ""); err != nil {
		t.Fatal(err)
	}
	ch := channel.NewLocal(root)
	task, _ := st.GetTask(id)
	if _, ok := acceptParkedReview(context.Background(), st, ch, repo, localCfg(), task); ok {
		t.Fatal("无分支记录的 accept 必须降级为重跑（ok=false），而非拦截")
	}
}

// TestHumanTierForByChannelCapability 核验 0b 接线：真 tier-3 只挂到有回复通道的
// provider（github/linear）；local 的 ListReplies 是 no-op，park 后无人能回复，
// 保持 stub（nil）。开关关 → nil。
func TestHumanTierForByChannelCapability(t *testing.T) {
	ch := channel.NewLocal(t.TempDir())
	on := func(prov string) *config.Config {
		return &config.Config{
			Verify:  config.Verify{Tier3Human: true},
			Channel: config.Channel{Provider: prov},
		}
	}
	if ht := humanTierFor(on("local"), ch, "1"); ht != nil {
		t.Fatal("local 无回复通道，必须保持 stub（nil）——park 会搁浅任务")
	}
	if ht := humanTierFor(on("github"), ch, "42"); ht == nil {
		t.Fatal("github + tier3_human=true 必须挂真 Human tier（此前生产从未接线，开关静默自动过）")
	} else if h, isHuman := ht.(*verify.Human); !isHuman || h.Ref != "42" {
		t.Fatalf("期望 *verify.Human{Ref:42}，got %+T", ht)
	}
	if ht := humanTierFor(on("linear"), ch, "L-1"); ht == nil {
		t.Fatal("linear + tier3_human=true 必须挂真 Human tier")
	}
	off := on("github")
	off.Verify.Tier3Human = false
	if ht := humanTierFor(off, ch, "42"); ht != nil {
		t.Fatal("tier3_human=false → nil（链上无 tier-3）")
	}
}
