#!/usr/bin/env bash
# The check set of this repo. `just check` and crate's merge queue both run this file, so green means the same in both.
# One set, code: its format, go vet and the tests. `scripts/check.sh all` and `scripts/check.sh code` run the same.
set -euo pipefail
cd "$(dirname "$0")/.."

case "${1:-all}" in
  all | code) ;;
  --plan) echo code; exit 0 ;;
  *) echo "usage: scripts/check.sh [all|code|--plan]" >&2; exit 2 ;;
esac

echo "== code"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "$unformatted"
  echo "gofmt: these files are not formatted" >&2
  exit 1
fi
go vet ./...
go test ./...
echo "check passed"
