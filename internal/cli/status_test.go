// internal/cli/status_test.go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"loop-eng/internal/state"
)

// TestStatusPrintsTask drives `loop-eng status` after a fake task is hand-
// inserted into state.db. The CLI must list each task_status row as
// "id status" without touching the Store's private db field (裁决 F: status
// goes through the public ListStatuses method).
func TestStatusPrintsTask(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// init 生成 .loop/state.db（status --repo 需要它）
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	// 手插一条任务（state.Open + InsertTask，不经 CLI）
	insertFakeTask(t, repo)

	var buf bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"status", "--repo", repo})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "running") && !strings.Contains(out, "new") && !strings.Contains(out, "task_") {
		t.Fatalf("status output empty: %q", out)
	}
}

// insertFakeTask opens <repo>/.loop/state.db and inserts one throwaway task
// row (status defaults to "new"). Helper for status command tests.
func insertFakeTask(t *testing.T, repo string) {
	t.Helper()
	st, err := state.Open(filepath.Join(repo, ".loop", "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	if _, err := st.InsertTask(state.TaskRow{
		Description: "fake task for status test",
		TaskType:    "bugfix",
	}); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

// TestRenderWatchView asserts the pure --watch renderer with an injected fake
// InFlight + status rows: the active task + phase show on top, every status
// below, and an empty active slot / blank phase degrade sensibly. No time, no
// signals — just the string (spec §8.7 live view).
func TestRenderWatchView(t *testing.T) {
	// active task + phase, plus a couple of status rows.
	got := renderWatchView(
		state.InFlight{TaskID: "task_aa", Phase: "execute"},
		[]state.StatusRow{
			{ID: "task_aa", Status: "running"},
			{ID: "task_bb", Status: "new"},
		},
	)
	for _, want := range []string{"task_aa", "phase=execute", "task_aa running", "task_bb new"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in frame:\n%s", want, got)
		}
	}

	// no active task → slot-empty line, no phase=.
	got = renderWatchView(state.InFlight{}, nil)
	if !strings.Contains(got, "无活跃任务") {
		t.Errorf("empty InFlight should mark no active task:\n%s", got)
	}
	if strings.Contains(got, "phase=") {
		t.Errorf("empty InFlight must not emit phase=:\n%s", got)
	}

	// dispatched but no step landed → Phase blank → falls back to "running".
	got = renderWatchView(state.InFlight{TaskID: "task_cc"}, nil)
	if !strings.Contains(got, "task_cc  phase=running") {
		t.Errorf("blank phase should fall back to running:\n%s", got)
	}
}

// TestWatchFlagRegistered asserts the --watch bool flag exists on `status` and
// defaults off, so the non-watch path is the default (spec §8.7: --watch is
// opt-in).
func TestWatchFlagRegistered(t *testing.T) {
	cmd := NewStatusCmd()
	flag := cmd.Flags().Lookup("watch")
	if flag == nil {
		t.Fatal("--watch flag not registered on status")
	}
	if flag.Value.Type() != "bool" {
		t.Fatalf("--watch type = %q want bool", flag.Value.Type())
	}
	if flag.DefValue != "false" {
		t.Fatalf("--watch default = %q want false", flag.DefValue)
	}
}

// TestWatchExitsOnSignal drives runWatch with a pre-fed SIGINT and a large
// interval: it must render exactly one frame then return nil on the signal
// (Ctrl-C exits; the watch never self-exits — spec §8.7). Deterministic: the
// hour-long ticker cannot fire before the ready signal.
func TestWatchExitsOnSignal(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	initCmd := NewRootCmd()
	initCmd.SetArgs([]string{"init", "--repo", repo})
	if err := initCmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}
	insertFakeTask(t, repo) // a "new" task so the status list is non-empty

	st := mustOpenState(repo)
	defer st.Close()

	var buf bytes.Buffer
	sigCh := make(chan os.Signal, 1)
	sigCh <- syscall.SIGINT
	if err := runWatch(st, &buf, time.Hour, sigCh); err != nil {
		t.Fatalf("runWatch returned error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "watch") {
		t.Fatalf("watch header not rendered:\n%s", out)
	}
	if !strings.Contains(out, "new") && !strings.Contains(out, "task_") {
		t.Fatalf("status list not rendered in watch frame:\n%s", out)
	}
}
