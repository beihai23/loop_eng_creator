#!/usr/bin/env bash
set -euo pipefail
DOC="docs/superpowers/specs/linear-channel-mapping.md"
[ -s "$DOC" ] || { echo "MISSING FILE: $DOC"; exit 1; }
need(){ grep -qi -- "$1" "$DOC" || { echo "MISSING TOKEN: $1"; exit 1; }; }
for o in Issue Team Project Cycle WorkflowState Workflow Comment Label; do need "$o"; done
for m in ListNewTasks ListReplies PostComment UpdateStatus CloseIssue GetTaskStates; do need "$m"; done
need "Authorization"
need "project"
need "land"
need "待人工核实"
grep -qi "query" "$DOC" || { echo "MISSING: graphql query example"; exit 1; }
grep -qi "mutation" "$DOC" || { echo "MISSING: graphql mutation example"; exit 1; }
echo "OK: linear-channel-mapping structure complete"