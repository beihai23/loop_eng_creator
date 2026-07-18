package daemon

import (
	"context"
	"testing"
	"time"

	"loop-eng/internal/channel"
	"loop-eng/internal/state"
)

// statusCall is one recorded UpdateStatus invocation.
type statusCall struct{ ref, status string }

// statusRecorder records every UpdateStatus call (ref + status, in order) so
// tests can assert the running mark lands at dispatch.
type statusRecorder struct {
	scriptedChannel
	calls []statusCall
}

func (r *statusRecorder) UpdateStatus(ctx context.Context, ref, status string) error {
	r.calls = append(r.calls, statusCall{ref: ref, status: status})
	return nil
}

// TestDispatchMarksRunning 钉死「派发即标正在处理」的落点：daemon tick 把任务
// new→running 派发时，必须在 channel 上打 loop:running（UpdateStatus(ref,
// "running")）——看板之外的人由此知道任务正在被处理。
func TestDispatchMarksRunning(t *testing.T) {
	st := newTestStore(t)
	ch := &statusRecorder{scriptedChannel: scriptedChannel{batches: [][]channel.Task{
		{{Ref: "42", Description: "task X", TaskType: "feat"}},
	}}}
	eng := &Engine{
		Channel: ch, Store: st, Interval: time.Second,
		RunTask: func(ctx context.Context, task state.TaskRow) (string, string, error) {
			return "done", "ok", nil
		},
	}
	if err := eng.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	for _, c := range ch.calls {
		if c.ref == "42" && c.status == "running" {
			return
		}
	}
	t.Fatalf("UpdateStatus calls = %v, want one with ref=42 status=running", ch.calls)
}
