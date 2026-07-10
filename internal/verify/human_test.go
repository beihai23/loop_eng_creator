package verify

import (
	"context"
	"time"

	"loop-eng/internal/channel"
)

var _ channel.Channel = (*fakeReviewChan)(nil)

type fakeReviewChan struct {
	comments map[string][]string
	statuses map[string]string
	err      error
}

func (f *fakeReviewChan) ListNewTasks(context.Context) ([]channel.Task, error) {
	return nil, nil
}
func (f *fakeReviewChan) ListReplies(context.Context, []string, time.Time) (map[string][]channel.Reply, error) {
	return nil, nil
}
func (f *fakeReviewChan) PostComment(_ context.Context, ref, body string) error {
	if f.err != nil {
		return f.err
	}
	if f.comments == nil {
		f.comments = map[string][]string{}
	}
	f.comments[ref] = append(f.comments[ref], body)
	return nil
}
func (f *fakeReviewChan) UpdateStatus(_ context.Context, ref, status string) error {
	if f.statuses == nil {
		f.statuses = map[string]string{}
	}
	f.statuses[ref] = status
	return nil
}
func (f *fakeReviewChan) CloseIssue(context.Context, string) error                            { return nil }
func (f *fakeReviewChan) GetTaskStates(context.Context, []string) (map[string]channel.TaskState, error) {
	return nil, nil
}
