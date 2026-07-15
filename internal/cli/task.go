// internal/cli/task.go
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// NewTaskCmd builds the `loop-eng task` parent command (spec §8.1). Currently has
// `new` — create a task file with language-aware criteria suggestions.
func NewTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "任务管理（创建、查看）",
	}
	cmd.AddCommand(newTaskNewCmd())
	return cmd
}

func newTaskNewCmd() *cobra.Command {
	var repo, taskType string
	cmd := &cobra.Command{
		Use:   "new <description>",
		Short: "创建任务文件（自动检测项目语言 + 建议验收标准）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			desc := strings.Join(args, " ")
			lang := detectLanguage(repo)
			verifyCmd, buildCmd, criteria := suggestCriteria(lang)

			content := fmt.Sprintf("# 任务\n%s\ntype: %s\n## 验收标准\n%s\n",
				desc, taskType, criteria)

			inboxDir := filepath.Join(repo, "inbox")
			if err := os.MkdirAll(inboxDir, 0755); err != nil {
				return err
			}
			n := nextInboxNumber(inboxDir)
			path := filepath.Join(inboxDir, fmt.Sprintf("%d.md", n))
			if err := os.WriteFile(path, []byte(content), 0644); err != nil {
				return err
			}

			fmt.Printf("Task created: %s\n", path)
			fmt.Printf("Detected language: %s\n", lang)
			if verifyCmd != "" {
				// tier-1 验收脚本由 plan 按任务产出（PlanOutput.verify_script），不再写进
				// config——这里只把检测到的测试命令作为信息提示。
				fmt.Printf("Suggested verify command: %s (plan 会据此自动产出 tier-1 验收脚本，无需写进 config)\n", verifyCmd)
			}
			if buildCmd != "" {
				fmt.Printf("Suggested build: %s\n", buildCmd)
			}
			fmt.Printf("\nEdit %s to adjust criteria, then:\n  loop-eng run-once --repo %s --channel local\n", path, repo)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", ".", "仓库路径")
	cmd.Flags().StringVar(&taskType, "type", "feature", "feature | bugfix | docs | refactor")
	return cmd
}

// detectLanguage inspects the repo for language markers (go.mod, package.json,
// Cargo.toml, pyproject.toml/setup.py, pom.xml). Returns "go"/"node"/"rust"/
// "python"/"java" or "unknown".
func detectLanguage(repo string) string {
	exists := func(f string) bool {
		_, err := os.Stat(filepath.Join(repo, f))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return "go"
	case exists("package.json"):
		return "node"
	case exists("Cargo.toml"):
		return "rust"
	case exists("pyproject.toml") || exists("setup.py"):
		return "python"
	case exists("pom.xml"):
		return "java"
	}
	return "unknown"
}

// suggestCriteria returns (verifyCmd, buildCmd, criteriaLines) for the detected
// language. The criteria are PROJECT-SPECIFIC suggestions from the wizard — the
// tool core itself stays language-agnostic (no hardcoded go test / pytest).
func suggestCriteria(lang string) (verifyCmd, buildCmd, criteria string) {
	switch lang {
	case "go":
		verifyCmd = "go test ./..."
		buildCmd = "CGO_ENABLED=0 go build ./..."
		criteria = "- [ ] CGO_ENABLED=0 go build ./... 成功\n" +
			"- [ ] go test ./... 全绿，不破坏现有测试\n" +
			"- [ ] 不引入不必要的依赖"
	case "python":
		verifyCmd = "pytest -q"
		criteria = "- [ ] pytest -q 全绿，不破坏现有测试\n" +
			"- [ ] 无新 lint 警告（ruff/flake8）\n" +
			"- [ ] 无安全问题（bandit）"
	case "node":
		verifyCmd = "npm test"
		buildCmd = "npm run build"
		criteria = "- [ ] npm test 全绿，不破坏现有测试\n" +
			"- [ ] npm run build 成功\n" +
			"- [ ] 无新 lint 警告（eslint）"
	case "rust":
		verifyCmd = "cargo test"
		buildCmd = "cargo build"
		criteria = "- [ ] cargo test 全绿，不破坏现有测试\n" +
			"- [ ] cargo build 成功\n" +
			"- [ ] 无新 clippy 警告"
	case "java":
		verifyCmd = "mvn test"
		buildCmd = "mvn compile"
		criteria = "- [ ] mvn test 全绿，不破坏现有测试\n" +
			"- [ ] mvn compile 成功\n" +
			"- [ ] 无新 checkstyle 警告"
	default:
		criteria = "- [ ] 满足任务描述的全部要求\n" +
			"- [ ] 不破坏现有功能\n" +
			"- [ ] 可脚本化的验收由 plan 产出 tier-1 脚本；不可脚本化的交 tier-2/tier-3"
	}
	return
}

// nextInboxNumber finds the next available task number in inbox/ (1, 2, 3, ...).
func nextInboxNumber(inboxDir string) int {
	max := 0
	entries, _ := os.ReadDir(inboxDir)
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		if n, err := strconv.Atoi(name); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}
