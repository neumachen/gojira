#!/usr/bin/env bash
set -euo pipefail

# Lightweight Aider lint/format hook for Go files.
# Aider may pass changed filenames as arguments.

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

has_go_mod=false
if [[ -f go.mod ]]; then
  has_go_mod=true
fi

go_files=()
for arg in "$@"; do
  if [[ "$arg" == *.go && -f "$arg" ]]; then
    go_files+=("$arg")
  fi
done

if (( ${#go_files[@]} > 0 )); then
  gofmt -w "${go_files[@]}"
fi

if [[ "$has_go_mod" == true ]]; then
  go vet ./...
fi
