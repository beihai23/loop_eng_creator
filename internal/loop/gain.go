// Package loop gain.go — the retry **gain gate** (零增益门槛).
//
// SubLoop's retry used to be a mechanical counter (attempt <= MaxRetries → run
// another round) that never asked whether this round carried any *new* signal
// over the last. Two real failure modes burned tokens on retries that could not
// self-heal (see docs/work-backstory):
//   - #71 oscillation (run_6ca678c5c8c42fbf): plan froze a signature, execute
//     returned a different one; verify bounced 4→3 → 3→2 → 2→3 each round, the
//     *numbers* differing but the *shape* identical. Three rounds ended with the
//     exact same information as the first.
//   - run_b154ff8213e7851a: plan returned "claude -p: context canceled" three
//     times, verbatim — zero new information across all three retries.
//
// This file supplies the deterministic core of the gate: a failure-signature
// normalizer that collapses the above two forms (oscillating numbers / verbatim
// repeats) to one signature while keeping distinct symbols distinct, plus the
// zeroGain predicate. The SubLoop wiring (when to call it, the escalate path)
// lives in subloop.go; the structured-help fallback for the blocked battle
// report lives here too.

package loop

import (
	"fmt"
	"regexp"
	"strings"

	"loop-eng/internal/channel"
	"loop-eng/internal/skill"
)

// failureSignature normalizes a failure message into a stable signature so two
// failures that differ only in non-signal noise (timestamps, run IDs, hex
// addresses, file paths, line numbers, bare counts) compare equal, while
// failures that differ in a real symbol stay distinct.
//
// Pipeline (order matters — each step peels a layer before the next):
//  1. lowercase (so VERB and verb, Foo and foo match).
//  2. strip timestamps — full ISO-8601 datetimes (the realistic log form), so a
//     timestamp of ANY internal format collapses to nothing rather than to a
//     separator skeleton that varies by format.
//  3. strip run IDs run_[0-9a-f]+ (e.g. run_b154ff82).
//  4. strip hex literals 0x[0-9a-f]+ (addresses).
//  5. strip slash-bearing paths (any whitespace-delimited token containing '/',
//     e.g. /a/b.go:42) — the whole token, line number and all.
//  6. strip remaining bare digit runs \d+ — this is the lever that collapses the
//     #71 oscillation: "execute 返回 4 个值" and "execute 返回 2 个值" both lose
//     the count and compare equal. It deliberately does NOT strip letters, so
//     "undefined: Foo" and "undefined: Bar" stay distinct (#46 normal retry).
//  7. collapse runs of whitespace to a single space, then TrimSpace.
//
// Empty/whitespace-only input returns "" (which zeroGain treats as "no signal,
// never escalate" — the first failure can never be zero-gain).
func failureSignature(s string) string {
	s = strings.ToLower(s)
	s = reTimestamp.ReplaceAllString(s, "")
	s = reRunID.ReplaceAllString(s, "")
	s = reHexLit.ReplaceAllString(s, "")
	s = rePathTok.ReplaceAllString(s, "")
	s = reDigits.ReplaceAllString(s, "")
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	// reTimestamp matches a full ISO-8601 datetime (the form timestamps take in
	// real logs): YYYY-MM-DD[T|space]hh:mm:ss with optional fractional seconds
	// and optional zone (Z / ±HH[:MM]). Matched whole so differing internal
	// formats normalize identically.
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:z|[+-]\d{2}:?\d{2})?`)
	// reRunID matches run_<hex> identifiers (run_b154ff82).
	reRunID = regexp.MustCompile(`run_[0-9a-f]+`)
	// reHexLit matches 0x-prefixed hex literals (pointer addresses).
	reHexLit = regexp.MustCompile(`0x[0-9a-f]+`)
	// rePathTok matches a maximal whitespace-delimited token that contains at
	// least one '/', e.g. /a/b.go:42 or src/pkg/file.go. \S stops at whitespace
	// so each path token is handled independently and the line number riding on
	// its tail goes with it.
	rePathTok = regexp.MustCompile(`\S*/\S*`)
	// reDigits matches any remaining run of digits (the #71 oscillation lever).
	reDigits = regexp.MustCompile(`\d+`)
	// reWhitespace collapses the gaps left by the peels into single spaces.
	reWhitespace = regexp.MustCompile(`\s+`)
)

// zeroGain reports whether the current failure is a zero-information repeat of
// the previous one: true only when BOTH signatures are non-empty AND equal.
//
// A first attempt (prevSig="") can never be zero-gain, and a round whose
// signature normalized to empty (no signal at all) can never trip the gate — so
// the gate never fires on a genuine first failure or on noise-only output.
func zeroGain(prevSig, curSig string) bool {
	return curSig != "" && curSig == prevSig
}

// retryGateTrace is the structured record SubLoop writes to a role="retry-gate"
// step's OutputJSON for every retry decision, so "凭什么这一轮值得跑" is auditable.
// zero_gain=true means the attempt was NOT retried but escalated (same failure
// signature as the previous attempt → a retry could not self-heal).
type retryGateTrace struct {
	ZeroGain      bool   `json:"zero_gain"`
	Signature     string `json:"signature"`
	PrevSignature string `json:"prev_signature"`
	NewReplySince bool   `json:"new_reply_since"`
	DiffChanged   bool   `json:"diff_changed"`
	Decision      string `json:"decision"`
	Attempt       int    `json:"attempt"`
}

// synthesizeHelp is the deterministic fallback for a zero-gain blocked battle
// report: when the help skill is not wired (Model == nil, the common test path)
// or its Run errors, SubLoop falls back to this so a blocked report ALWAYS
// carries a non-empty stuck_at / tried / need_from_human. It quotes a clipped
// summary of the prior failure, names that N retries all failed with the same
// signature, and asks the human to decide on the contract / supply info /
// troubleshoot the environment.
func synthesizeHelp(task channel.Task, attempt int, priorFailure string) skill.HelpOutput {
	var out skill.HelpOutput
	var stuck strings.Builder
	stuck.WriteString("重试零增益：连续两轮失败签名相同，重试不会自愈")
	if d := strings.TrimSpace(task.Description); d != "" {
		stuck.Reset()
		stuck.WriteString("任务「" + truncateStr(d, 80) + "」重试零增益：连续两轮失败签名相同，重试不会自愈")
	}
	if pf := strings.TrimSpace(priorFailure); pf != "" {
		stuck.WriteString("。上一轮失败摘要：" + truncateStr(pf, 200))
	}
	out.HelpRequest.StuckAt = stuck.String()
	out.HelpRequest.Tried = []string{
		fmt.Sprintf("已重试 %d 轮，均以相同失败签名告终（机械重试未带来新增有效信息）", attempt),
	}
	out.HelpRequest.NeedFromHuman = "请人决策其一：核实/修订实现合同（函数签名、接口契约）、补充关键信息，" +
		"或排查运行环境（上游限流/超时/基础设施）。在本 issue 回复后，daemon 会重排队并携带新信息重试。"
	return out
}

// formatHelp renders a HelpOutput into the human-visible section SubLoop appends
// to a zero-gain blocked detail. The literal field tags stuck_at / tried /
// need_from_human are intentional — they let a human (and tests) grep the
// structured-help fields straight out of the battle report.
func formatHelp(out skill.HelpOutput) string {
	var b strings.Builder
	b.WriteString("stuck_at: " + strings.TrimSpace(out.HelpRequest.StuckAt) + "\n")
	b.WriteString("tried:")
	if len(out.HelpRequest.Tried) == 0 {
		b.WriteString(" (无记录)\n")
	} else {
		b.WriteString("\n")
		for _, t := range out.HelpRequest.Tried {
			b.WriteString("- " + t + "\n")
		}
	}
	b.WriteString("need_from_human: " + strings.TrimSpace(out.HelpRequest.NeedFromHuman))
	return b.String()
}
