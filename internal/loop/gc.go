package loop

// gc.go —— worktree 现场的状态驱动 GC。
//
// 设计前提（与 scene.go 一体两面）：失败现场的**档案**在 SQLite（steps.output_json，
// append-only、可回放），worktree 只是给人排查/复用的**缓存**。缓存的 GC 不需要
// 聪明——最坏的误删丢的是调试便利，永远丢不了档案。所以策略刻意简单：
//
//   - 宽限期（默认 48h）内的树一律不碰——防状态滞后误删活树（运行中的 attempt、
//     刚 park 的现场，status 还没来得及反映）。
//   - 过了宽限期再问状态（task_status 是唯一真相源）：
//     running / needs-review / needs-info / needs-human-decision → 活树，保留；
//     done → 保留（land 失败的 salvage 现场，罕见，留给人）；
//     blocked → 失败现场缓存，SceneTTL（默认 7d）到期删；
//     其余（cancelled / error / 无 task 行的孤儿 / 名字无法解析）→ 删。
//
// 触发点：daemon 每 tick 一次（本文件 GCWorktrees）+ `loop-eng clean`（cli 层）。

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"loop-eng/internal/isolation"
	"loop-eng/internal/state"
)

// GC 默认策略常量。宽限期是用户拍板的 48h；SceneTTL 是失败现场缓存的保留时长。
const (
	DefaultGCGrace   = 48 * time.Hour
	DefaultGCSceneTTL = 7 * 24 * time.Hour
)

// GCPolicy 是 GCWorktrees 的判定参数。Now 注入便于测试（不传用 time.Now）。
type GCPolicy struct {
	Now      time.Time
	Grace    time.Duration // 宽限期：此年龄内的树一律保留（防状态滞后误删活树）
	SceneTTL time.Duration // blocked 现场缓存的保留时长
	DryRun   bool          // 只判定不删除（loop-eng clean --dry-run）
}

// DefaultGCPolicy 返回生产默认策略（Now=调用时刻）。
func DefaultGCPolicy() GCPolicy {
	return GCPolicy{Now: time.Now(), Grace: DefaultGCGrace, SceneTTL: DefaultGCSceneTTL}
}

// GCAction 记录 GC 对一棵树的决定（保留/删除 + 理由），供观测与 dry-run 输出。
type GCAction struct {
	Name   string
	Delete bool
	Reason string
}

// sceneTaskID 从 worktree 目录名 <taskID>-r<attempt> 解析 taskID。taskID 本身
// 不含 "-r" 段（newID 产 hex），取最后一个 "-r" 且后缀为纯数字才认。
func sceneTaskID(name string) (string, bool) {
	i := strings.LastIndex(name, "-r")
	if i <= 0 || i+2 >= len(name) {
		return "", false
	}
	if _, err := strconv.Atoi(name[i+2:]); err != nil {
		return "", false
	}
	return name[:i], true
}

// GCWorktrees 按 GCPolicy 清理 repo 的 .loop/worktrees。返回每棵树的决定
// （含保留理由，供日志/dry-run）。单棵删除失败不中断——记进结果继续。
func GCWorktrees(repo string, st *state.Store, pol GCPolicy) ([]GCAction, error) {
	if pol.Now.IsZero() {
		pol.Now = time.Now()
	}
	entries, err := isolation.List(repo)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// 状态真相源：task_status 全量（量小，一次取完建 map，避免逐树查库）。
	statuses := map[string]string{}
	if st != nil {
		rows, err := st.ListStatuses()
		if err != nil {
			return nil, fmt.Errorf("gc: list statuses: %w", err)
		}
		for _, r := range rows { // ORDER BY updated_at → 后写覆盖，map 里是最新态
			statuses[r.ID] = r.Status
		}
	}
	var actions []GCAction
	for _, e := range entries {
		act := decide(e, statuses, pol)
		if act.Delete && !pol.DryRun {
			if err := isolation.Prune(repo, e.Path); err != nil {
				act = GCAction{Name: e.Name, Delete: false, Reason: "delete failed: " + err.Error()}
			}
		}
		actions = append(actions, act)
	}
	return actions, nil
}

// decide 是纯判定（不碰文件系统/git），便于单测逐分支钉死。
func decide(e isolation.Entry, statuses map[string]string, pol GCPolicy) GCAction {
	age := pol.Now.Sub(e.ModTime)
	if age < pol.Grace {
		return GCAction{Name: e.Name, Reason: fmt.Sprintf("within grace (%s < %s)", age.Round(time.Hour), pol.Grace)}
	}
	taskID, ok := sceneTaskID(e.Name)
	if !ok {
		return GCAction{Name: e.Name, Delete: true, Reason: "unparseable name (orphan)"}
	}
	status, known := statuses[taskID]
	if !known {
		return GCAction{Name: e.Name, Delete: true, Reason: "no task row (orphan)"}
	}
	switch status {
	case "running", "needs-review", "needs-info", "needs-human-decision":
		return GCAction{Name: e.Name, Reason: "live task (" + status + ")"}
	case "done":
		return GCAction{Name: e.Name, Reason: "done (land-salvage, left for operator)"}
	case "blocked":
		if age > pol.SceneTTL {
			return GCAction{Name: e.Name, Delete: true, Reason: fmt.Sprintf("blocked scene past TTL (%s > %s)", age.Round(time.Hour), pol.SceneTTL)}
		}
		return GCAction{Name: e.Name, Reason: "blocked scene within TTL"}
	default: // cancelled / error / 未来新终态——默认删（缓存而已，档案在 SQLite）
		return GCAction{Name: e.Name, Delete: true, Reason: "terminal status (" + status + ")"}
	}
}
