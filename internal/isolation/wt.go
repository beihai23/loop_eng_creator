package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0644)
}

// Create 在 baseRepo 旁的 .loop/worktrees/<runID> 建 worktree，返回其绝对路径。
func Create(baseRepo, runID string) (string, error) {
	wtRoot := filepath.Join(baseRepo, ".loop", "worktrees")
	if err := os.MkdirAll(wtRoot, 0755); err != nil {
		return "", err
	}
	wtPath := filepath.Join(wtRoot, runID)
	branch := "loop/" + runID
	// -B（不是 -b）：分支已存在时强制重置到 HEAD，而非报 "already exists" 崩掉。
	// 同一 <taskID>-r<attempt> 分支可能因上次 run 在 Discard 前崩溃、land 失败、
	// 或手工操作而残留——-b 会让下一次 Create 直接 error（#20 就栽在这）。
	out, err := exec.Command("git", "-C", baseRepo,
		"worktree", "add", "-B", branch, wtPath, "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %s", out)
	}
	// filepath.Join stays relative when baseRepo == "."; resolve to absolute so
	// the doc comment ("返回其绝对路径") is truthful and downstream git -C calls
	// work regardless of the caller's cwd.
	return filepath.Abs(wtPath)
}

// Discard 删 worktree 及其分支。
func Discard(baseRepo, wtPath string) error {
	out, err := exec.Command("git", "-C", baseRepo, "worktree", "remove", "--force", wtPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree remove: %s", out)
	}
	branch := "loop/" + filepath.Base(wtPath)
	out, err = exec.Command("git", "-C", baseRepo, "branch", "-D", branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git branch -D: %s", out)
	}
	return nil
}
