package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
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

// Entry 是 .loop/worktrees 下一棵 worktree 的枚举项（GC 的输入）。
type Entry struct {
	Name    string    // 目录名（<taskID>-r<attempt>）
	Path    string    // 绝对路径
	ModTime time.Time // 目录 mtime——GC 的年龄依据（宽限期/TTL 都拿它比较）
}

// List 枚举 .loop/worktrees 下的全部 worktree 目录（不存在时返回空，非错误）。
// 只读文件系统，不碰 git——GC 的策略层据此决定哪些交给 Prune。
func List(baseRepo string) ([]Entry, error) {
	wtRoot := filepath.Join(baseRepo, ".loop", "worktrees")
	des, err := os.ReadDir(wtRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		abs, _ := filepath.Abs(filepath.Join(wtRoot, de.Name()))
		out = append(out, Entry{Name: de.Name(), Path: abs, ModTime: fi.ModTime()})
	}
	return out, nil
}

// Prune 是 GC 专用的宽容版 Discard：worktree 移除失败（如目录已被手工删掉、
// 不是注册的 worktree）时回落 os.RemoveAll；分支删除 best-effort（分支不存在
// 不算错误——孤儿现场的分支可能早已没建或已被清）。
func Prune(baseRepo, wtPath string) error {
	if out, err := exec.Command("git", "-C", baseRepo, "worktree", "remove", "--force", wtPath).CombinedOutput(); err != nil {
		// 回落：目录可能不是注册的 worktree（手工复制/半残留）——直接删目录，
		// 再用 git worktree prune 清掉 git 侧的登记（如有）。
		if rmErr := os.RemoveAll(wtPath); rmErr != nil {
			return fmt.Errorf("git worktree remove: %s; fallback rmdir: %v", out, rmErr)
		}
		_ = exec.Command("git", "-C", baseRepo, "worktree", "prune").Run()
	}
	branch := "loop/" + filepath.Base(wtPath)
	// best-effort：分支不存在（"not found"）是孤儿现场的常态，不算失败。
	_ = exec.Command("git", "-C", baseRepo, "branch", "-D", branch).Run()
	return nil
}
