#!/usr/bin/env bash
set -euo pipefail
f=.github/workflows/ci.yml
test -f "$f" || { echo "MISSING $f"; exit 1; }
grep -Eq '^on:' "$f" || { echo 'no on:'; exit 1; }
grep -Eq 'pull_request' "$f" || { echo 'no pull_request trigger'; exit 1; }
grep -Eq 'push' "$f" || { echo 'no push trigger'; exit 1; }
grep -Eq 'main' "$f" || { echo 'push not scoped to main'; exit 1; }
grep -Eq 'go vet' "$f" || { echo 'no go vet step'; exit 1; }
grep -Eq 'go build' "$f" || { echo 'no go build step'; exit 1; }
grep -Eq 'go test' "$f" || { echo 'no go test step'; exit 1; }
grep -Eq 'CGO_ENABLED.*0' "$f" || { echo 'CGO not disabled'; exit 1; }
grep -Eq 'go-version-file: go.mod' "$f" || { echo 'go version not pinned to go.mod'; exit 1; }
grep -Eq 'cache: true' "$f" || { echo 'no setup-go cache'; exit 1; }
grep -Eq 'timeout-minutes' "$f" || { echo 'no timeout-minutes'; exit 1; }
if grep -vE '^[[:space:]]*#' "$f" | grep -Eq 'LOOP_ENG_AGENT_SMOKE[[:space:]]*[:=][[:space:]]*1'; then
  echo 'FAIL: CI enables LOOP_ENG_AGENT_SMOKE=1 (smoke must stay opt-in)'
  exit 1
fi
grep -Eq 'actions/workflows/ci.yml/badge' README.md || { echo 'no CI badge in README'; exit 1; }
echo 'ci.yml contract OK'