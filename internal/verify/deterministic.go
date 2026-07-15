package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Deterministic is the tier-1 check: run a command in the worktree (Dir) and
// judge by exit code + output. It is the runtime executor for the plan-produced
// acceptance script (skill.PlanVerifyScript) — there is no static config list
// behind it; every tier-1 comes from a PlanOutput.
//
// When ScriptBody is non-empty, Check first writes it to <Dir>/<ScriptFile> so
// the planner can ship a multi-line script (tech stack chosen by plan) and a Run
// command that executes it. A pure command (e.g. "go test ./...") leaves
// ScriptBody empty — nothing is written, Run just runs.
type Deterministic struct {
	Label string
	Cmd   []string
	Dir   string // worktree 路径

	// ScriptFile + ScriptBody：plan 产出的多行验收脚本。两者皆非空时，Check 先把
	// Body 写入 Dir/ScriptFile 再跑 Cmd。纯命令时留空，不落盘。
	ScriptFile string
	ScriptBody string
}

func (d Deterministic) Check(ctx context.Context, _ string, _ []string, _ string) (VerifyResult, error) {
	label := d.label()
	// plan 产出的脚本正文：先落盘到 worktree 再跑（纯命令时 ScriptBody 空，跳过）。
	if strings.TrimSpace(d.ScriptBody) != "" && d.ScriptFile != "" && d.Dir != "" {
		if err := os.WriteFile(filepath.Join(d.Dir, d.ScriptFile), []byte(d.ScriptBody), 0755); err != nil {
			return VerifyResult{Passed: false, Detail: label + ": write script: " + err.Error()}, nil
		}
	}
	if len(d.Cmd) == 0 {
		return VerifyResult{Passed: false, Detail: label + ": empty run command"}, nil
	}
	cmd := exec.CommandContext(ctx, d.Cmd[0], d.Cmd[1:]...)
	if d.Dir != "" {
		cmd.Dir = d.Dir
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return VerifyResult{Passed: true, Detail: label + ": ok"}, nil
	}
	return VerifyResult{Passed: false, Detail: fmt.Sprintf("%s: %s", label, out.String())}, nil
}

// label returns the observability tag, defaulting to "tier-1" when the planner
// left Label blank.
func (d Deterministic) label() string {
	if d.Label != "" {
		return d.Label
	}
	return "tier-1"
}
