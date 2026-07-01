#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

if [[ -f Makefile ]] && grep -Eq '^(test|check):' Makefile; then
  if grep -Eq '^test:' Makefile; then
    make test
  else
    make check
  fi
  exit 0
fi

if [[ -f go.mod ]]; then
  go test ./...
  exit 0
fi

echo "No go.mod or Makefile test/check target found; nothing to test."
