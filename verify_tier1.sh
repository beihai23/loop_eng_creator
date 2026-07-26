#!/usr/bin/env bash
set -euo pipefail
SPEC="docs/superpowers/specs/2026-07-02-loop-eng-core-design.md"
AGENT="internal/model/agent.go"

# 从 agent.go 的 Providers 注册表提取真实 provider key（自校准：§8.10 必须列出全部）
map_block=$(sed -n '/var Providers = map/,/^}/p' "$AGENT")
keys=$(printf '%s\n' "$map_block" | grep -oE '"[a-z]+"' | tr -d '"' | sort -u || true)
if [ -z "$keys" ]; then echo "FAIL: 提取 Providers 注册表为空（检查 sed 锚点 / agent.go）"; exit 1; fi
echo "registry providers: $(printf '%s\n' "$keys" | tr '\n' ' ')"

# §8.10 区域必须出现注册表里的每一个 provider
section=$(sed -n '/^### 8.10/,/^### 8.11/p' "$SPEC")
if [ -z "$section" ]; then echo "FAIL: 未找到 §8.10 区域"; exit 1; fi
miss=0
for k in $keys; do
  printf '%s' "$section" | grep -q -- "$k" || { echo "FAIL: §8.10 未提及注册表 provider '$k'"; miss=1; }
done
[ "$miss" -eq 0 ] || exit 1

# §8.10 标题行不得再是『单路径』框架；与实现直接矛盾的『唯一真实实现』须从 spec 移除
if printf '%s' "$section" | sed -n '1p' | grep -q '单路径'; then echo "FAIL: §8.10 标题仍是 '单路径' 框架"; exit 1; fi
if grep -q -- '唯一真实实现' "$SPEC"; then echo "FAIL: 仍残留矛盾句 '唯一真实实现'"; exit 1; fi
# §8.10 须提及 Providers 注册表（单一真相源）
printf '%s' "$section" | grep -q 'Providers' || { echo "FAIL: §8.10 未提及 Providers 注册表"; exit 1; }

echo "OK: §8.10 provider 清单 = 注册表（$(printf '%s\n' "$keys" | tr '\n' ' ')）；已知矛盾句已移除"