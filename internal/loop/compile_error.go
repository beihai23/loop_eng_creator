package loop

import "strings"

// compileErrorMarkers 是 verify 驳回 detail 里「编译/构建错误」的机械特征。命中任一即
// 判定该轮失败属于编译/构建类（而非 tier-2 语义驳回）。集子保守：只收编译器/构建器真正
// 会吐出的串，避免把 tier-2 的自然语言 reason 误判成编译错误。
//
// 为什么用字符串特征而非解析：tier-1（Deterministic）的 detail 是验收命令的原始
// stdout/stderr（如 `go test` 的 `undefined: Foo` / `build failed`），机械可判、零歧义；
// tier-2 的 detail 是 LLM 的自然语言 reason，几乎不会含这些标记。这恰好把「编译错误」
// 从「语义驳回」里干净分开——这正是 #46 的失败模式：编译错误被混进战报散文后信号被稀释，
// execute 连续多轮不修。
var compileErrorMarkers = []string{
	"undefined:",                    // Go: undefined symbol（#46 的典型形态）
	"undeclared",                    // 通用：undeclared name
	"cannot use",                    // Go: type mismatch
	"syntax error",                  // 编译期语法错
	"exit status",                   // go run/test runner 非零退出
	"build failed",                  // go build/test 构建失败
	"error:",                        // 通用编译器错误前缀
	"not enough arguments in call to",
	"too many arguments in call to",
}

// looksLikeCompileError 判定一段 verify 驳回 detail 是否属于编译/构建类错误：命中任一
// compileErrorMarkers 即真。纯函数、机械判定——供 SubLoop 在 verify 驳回后把该 detail
// 作为「结构化的一手编译错误信号」与普通语义驳回区分开。
func looksLikeCompileError(detail string) bool {
	for _, m := range compileErrorMarkers {
		if strings.Contains(detail, m) {
			return true
		}
	}
	return false
}

// compileErrorSection 把一段编译/构建错误的原始 detail 包装成 execute prompt 里独立且
// 显眼的「⚠️ 上一轮编译错误（必须先修复）」段，逐字引用原始错误文本。
//
// 仅当 detail 非空且 looksLikeCompileError(detail) 为真时返回非空段；否则返回空串
// （调用方据此决定是否注入）——保证普通语义驳回（无编译特征）不触发该段。
//
// 这是 #46 的直接修复：编译错误原本要绕 verify detail → verifyFailComment 发 issue →
// 下一轮 collectIssueComments 读回 → 混进 execute prompt 的战报散文段 才到 execute，
// 信号被严重稀释。现在作为结构化一手信号、独立段直达 execute prompt 的显眼位置。
func compileErrorSection(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" || !looksLikeCompileError(detail) {
		return ""
	}
	return "⚠️ 上一轮编译错误（必须先修复——声称完成前先在 worktree 自检 go build / go test）:\n" +
		detail + "\n"
}
