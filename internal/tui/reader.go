package tui

import (
	"loop-eng/internal/config"
	"loop-eng/internal/state"
)

// Snapshot 是一次数据 tick 读出的全量视图（纯数据，供 view 函数消费）。
type Snapshot struct {
	Tasks   []state.TaskView // running 已浮顶（见 ReadSnapshot）
	Running *RunningInfo     // 进行中任务（in_flight 非空时），nil 表示无活跃
	Counts  map[string]int   // 各状态计数（new/running/needs-review/blocked/done/cancelled）
}

// RunningInfo 是当前活跃子 loop 的展示信息。
type RunningInfo struct {
	TaskID, Phase, StartedAt, RunID string
	Retry int // 当前 attempt（budget_ledger 最后一条 retry 行的 amount；1=首次，>1=重试中）
}

// ReadSnapshot 从 Store 读一次全量视图。running 任务（in_flight）从 TasksByStatus
// 列表里挑出来浮顶（TasksByStatus 本身把 running 排末尾，spec §5[1] 要它置顶）。
// cfg 用于详情页的验收方式派生（B5）；此处仅透传，不在此读取。
func ReadSnapshot(st *state.Store, cfg *config.Config) (*Snapshot, error) {
	tasks, err := st.TasksByStatus()
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, t := range tasks {
		counts[t.Status]++
	}

	snap := &Snapshot{Tasks: tasks, Counts: counts}

	// running 浮顶
	ifl, ok, err := st.InFlight()
	if err == nil && ok {
		runID, startedAt, rok, _ := st.ActiveRun(ifl.TaskID)
		ri := &RunningInfo{TaskID: ifl.TaskID, Phase: ifl.Phase, StartedAt: startedAt}
		if rok {
			ri.RunID = runID
			// retry：budget_ledger 最后一条 retry 行的 amount = 当前 attempt（>1 即重试中）。
			// 与 detail.go 同源，best-effort：读失败/无行 → Retry 留 0（总览按「首次」渲染）。
			if rows, e := st.BudgetLedger(runID); e == nil {
				for i := len(rows) - 1; i >= 0; i-- {
					if rows[i].Kind == "retry" {
						ri.Retry = rows[i].Amount
						break
					}
				}
			}
		}
		snap.Running = ri
		// 把 running 任务挪到列表最前
		for i, t := range snap.Tasks {
			if t.ID == ifl.TaskID {
				snap.Tasks = append([]state.TaskView{t}, append(snap.Tasks[:i], snap.Tasks[i+1:]...)...)
				break
			}
		}
		counts["running"] = 1
	}
	_ = cfg
	return snap, nil
}
