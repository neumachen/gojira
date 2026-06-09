#!/usr/bin/env bash
set -euo pipefail

# Proto codegen pipeline for gojira.
#
# Order matters:
#   1. lint     — style/structure checks (buf.yaml `lint`).
#   2. breaking — compare the working-tree proto against the last
#                 committed state on main, so accidental
#                 wire-incompatible changes (removed fields, renumbered
#                 tags, changed types) fail loudly before regeneration.
#                 The baseline is the committed `main` branch; the rule
#                 set is buf.yaml `breaking: use: [FILE]`.
#   3. generate — (re)produce the Go stubs under gen/.
#
# The breaking check is skipped automatically when the `main` branch is
# not available in the local git repo (e.g. a shallow CI checkout or a
# fresh clone without the ref). This keeps the script usable everywhere
# while still guarding the common local-dev and full-checkout cases.

buf lint

if git rev-parse --verify --quiet refs/heads/main >/dev/null; then
  buf breaking --against '.git#branch=main'
else
  echo "gen-proto: skipping 'buf breaking' (no local 'main' branch to compare against)"
fi

buf generate

echo "Proto codegen complete. Generated files in gen/"
