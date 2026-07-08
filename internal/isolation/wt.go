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
	out, err := exec.Command("git", "-C", baseRepo,
		"worktree", "add", "-b", branch, wtPath, "HEAD").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git worktree add: %s", out)
	}
	return wtPath, nil
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
