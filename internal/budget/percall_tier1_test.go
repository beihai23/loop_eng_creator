package budget

import (
	"context"
	"errors"
	"testing"

	"loop-eng/internal/model"
)

// TestTier1PerCallGateReal: 记录超限真实用量后，下一次 Estimate(role) 超 PerCall，
// 故 BeforeCall 对 plan/execute/verify 三角色都真实返回 ErrPerCall。
// 消灭「恒 PerCall estimate」与「estimate==PerCall」两处自欺。
func TestTier1PerCallGateReal(t *testing.T) {
	for _, role := range []string{"plan", "execute", "verify"} {
		e := New(100000, 1_000_000_000, 3) // PerCall=100000
		// 首次（无历史）取角色 floor：floor 须 <= PerCall 故首调用过。
		if err := e.BeforeCall(e.Estimate(role)); err != nil {
			t.Fatalf("%s: 首次 floor estimate=%d 应过 PerCall=100000, got %v", role, e.Estimate(role), err)
		}
		// 观察到超限真实用量（200000 > PerCall 100000）。
		e.Record(role, model.Usage{TokensIn: 150000, TokensOut: 50000})
		// 下次 estimate = 200000 * safety(>=1) >= 200000 > 100000 → 必拒。
		if err := e.BeforeCall(e.Estimate(role)); !errors.Is(err, ErrPerCall) {
			t.Fatalf("%s: 记录 200000 真实用量后 Estimate=%d 应触发 PerCall=100000, got %v", role, e.Estimate(role), err)
		}
	}
}

// TestTier1ClientRejectsWithoutCallingBase: budget.Client 经 Estimate(Role) 预检，
// 角色上次用量超 PerCall 时直接拒、不调用 Base —— 闭合 verify 的同义反复。
// 同一装饰器现也背 triage/help。FakeClient 对无前缀匹配返回非 ErrPerCall 错，
// 故若 Base 被调会得到非 ErrPerCall 错；要求 ErrPerCall 即证明 Base 未被调。
func TestTier1ClientRejectsWithoutCallingBase(t *testing.T) {
	enf := New(100000, 1_000_000_000, 3)
	enf.Record("verify", model.Usage{TokensIn: 150000, TokensOut: 50000}) // prime 200000
	base := model.NewFake(map[string]string{"match": "ok"})
	c := &Client{Base: base, Enf: enf, Role: "verify"}
	if _, _, err := c.Call(context.Background(), "nomatch"); !errors.Is(err, ErrPerCall) {
		t.Fatalf("超限时 want ErrPerCall（不得调 Base）, got %v", err)
	}
}

// TestTier1TriageHelpAccrueToTaskGate: triage/help 经 budget.Client 把 token 累进
// 共享 Enforcer.spent，故 per-task 闸可见（过去它们完全 bypass 预算）。
func TestTier1TriageHelpAccrueToTaskGate(t *testing.T) {
	for _, role := range []string{"triage", "help"} {
		enf := New(1_000_000, 100_000_000, 3)
		base := model.NewFake(map[string]string{"hi": "yo"}) // usage = 2+2=4（非零）
		c := &Client{Base: base, Enf: enf, Role: role}
		if _, _, err := c.Call(context.Background(), "hi"); err != nil {
			t.Fatalf("%s call: %v", role, err)
		}
		if enf.Spent() == 0 {
			t.Fatalf("%s: token 未累进 Enforcer —— per-task 闸对 %s 失明", role, role)
		}
	}
}
