package channel

// botcomment.go —— daemon 自发评论（战报/驳回/分诊/人审请求/land 注记）的识别与标记。
//
// 背景：issue 评论线程是「人可读写回」通道——VERIFY-FAIL 战报、DONE/BLOCKED 终态、
// NEEDS-INFO 分诊等都是 daemon 写给操作员看的。但 SubLoop.collectIssueComments 过去
// 把整条线程无过滤读回喂给 plan/execute，daemon 读到了自己的回声（同一条驳回理由
// 以散文形式第二次进 prompt，且随轮次无界膨胀）。本文件给出确定性识别杠杆：
//
//   - 新评论：发出时经 MarkBotComment 加 BotMarker 隐形标记（HTML 注释，issue 页面
//     不可见），IsBotComment 以标记为准。
//   - 存量评论（标记引入前已发出的）：IsBotComment 回落到 botPrefixes 前缀匹配——
//     每个产出点的前缀都是代码里钉死的常量，见各函数注释。
//
// 人写的评论不以这些前缀开头（人不会恰好用「BLOCKED: 」起头写反馈）；误滤的代价
// 也只是一条人反馈没进 prompt，不影响正确性（落地闸门是 verify，不是 prompt 内容）。

import "strings"

// BotMarker 是 daemon 自发评论携带的隐形标记（HTML 注释，渲染后不可见）。
// IsBotComment 的首要判据；放文件级常量以便测试与将来其他产出方复用。
const BotMarker = "<!-- loop-eng:bot -->"

// botPrefixes 是标记引入前存量 daemon 评论的确定性前缀（兜底判据）。与产出点一一对应：
//   - "## VERIFY-FAIL"          loop.verifyFailComment（每轮 verify 驳回战报）
//   - "## REVIEW-REQUEST"       verify.Human.reviewRequest（tier-3 人审请求）
//   - "DONE:"/"BLOCKED:"/"NEEDS-REVIEW:"/"CANCELLED:"
//                               loop.SubLoop.report（strings.ToUpper(status)+": "）
//   - "NEEDS-INFO:"/"NEEDS-HUMAN-DECISION:"
//                               daemon.parkByTriage 的两个评论模板
//   - "PR 待合并："              cli.finalizeLand（PR 创建成功、待合并）
//   - "[LAND PARTIAL:"          cli.landFallback（push 失败、工作仅本地）
var botPrefixes = []string{
	"## VERIFY-FAIL",
	"## REVIEW-REQUEST",
	"DONE:", "BLOCKED:", "NEEDS-REVIEW:", "CANCELLED:",
	"NEEDS-INFO:", "NEEDS-HUMAN-DECISION:",
	"PR 待合并：", "[LAND PARTIAL:",
}

// MarkBotComment 给 daemon 自发评论加 BotMarker（幂等：已带标记的原样返回）。
// 标记放评论首行——IsBotComment 的 Contains 判据不依赖位置，但首行对人类读者
// 查看原始文本时最不妨碍阅读。
func MarkBotComment(body string) string {
	if strings.Contains(body, BotMarker) {
		return body
	}
	return BotMarker + "\n" + body
}

// IsBotComment 报告一条 issue 评论是否 daemon 自发（战报/驳回/分诊/人审/land 注记）。
// 判据顺序：BotMarker（新评论，权威）→ botPrefixes 前缀（存量评论，兜底）。
func IsBotComment(body string) bool {
	if strings.Contains(body, BotMarker) {
		return true
	}
	t := strings.TrimSpace(body)
	for _, p := range botPrefixes {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	return false
}
