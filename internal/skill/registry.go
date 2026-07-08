package skill

// Registry 仅用于 CLI skill edit/test：name → 版本 + 默认模板来源。
// 强类型调用在 subloop 里直接用 Skill[I,O]，不经 Registry。
type Entry struct {
	Name, Version, DefaultEmbedPath string
}

// Defaults lists the four built-in skills with their version and the embed
// path (relative to repo root) of the placeholder prompt template. Task 13's
// cli package embeds these via //go:embed internal/cli/embed/skills/*.md, so
// the files must live under internal/cli/embed/skills/.
var Defaults = []Entry{
	{Name: "triage", Version: "1", DefaultEmbedPath: "internal/cli/embed/skills/triage.md"},
	{Name: "plan", Version: "1", DefaultEmbedPath: "internal/cli/embed/skills/plan.md"},
	{Name: "verify", Version: "1", DefaultEmbedPath: "internal/cli/embed/skills/verify.md"},
	{Name: "help", Version: "1", DefaultEmbedPath: "internal/cli/embed/skills/help.md"},
}
