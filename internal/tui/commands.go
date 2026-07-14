package tui

import "loop-eng/internal/state"

// issueResume 写一条 resume 命令（payload 可为空）。
func issueResume(st *state.Store, taskID, payload string) error {
	return st.InsertCommand(taskID, "resume", payload)
}

// issueCancel 写一条 cancel 命令。
func issueCancel(st *state.Store, taskID string) error {
	return st.InsertCommand(taskID, "cancel", "")
}
