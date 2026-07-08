package verify

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

type Deterministic struct {
	Label string
	Cmd   []string
	Dir   string // worktree 路径
}

func (d Deterministic) Check(ctx context.Context, _ string, _ []string, _ string) (VerifyResult, error) {
	cmd := exec.CommandContext(ctx, d.Cmd[0], d.Cmd[1:]...)
	if d.Dir != "" {
		cmd.Dir = d.Dir
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return VerifyResult{Passed: true, Detail: d.Label + ": ok"}, nil
	}
	return VerifyResult{Passed: false, Detail: fmt.Sprintf("%s: %s", d.Label, out.String())}, nil
}
