#!/usr/bin/env bash
# create-release.sh — automate the full gojira release flow via GitHub CLI.
#
# USAGE
#   ./scripts/create-release.sh <VERSION>
#   ./scripts/create-release.sh <VERSION> --dry-run
#
# EXAMPLES
#   ./scripts/create-release.sh v0.4.1
#   ./scripts/create-release.sh v0.5.0 --dry-run
#
# WHAT THIS SCRIPT DOES
#   1. Validates preconditions (gh installed, on main, clean tree, tag unused)
#   2. Stamps internal/buildinfo/buildinfo.go with the commit SHA and VERSION
#   3. Commits the stamped file
#   4. Tags the commit
#   5. Pushes to origin (main + tag)
#   6. Creates a GitHub release with auto-generated notes
#
# DRY-RUN MODE
#   Pass --dry-run as the second argument to perform local stamping, commit,
#   and tag without pushing or creating the GitHub release. Useful for
#   inspecting the result before going live. To undo a dry-run:
#
#       git tag -d <VERSION>
#       git reset --hard HEAD~1
#
# ERROR RECOVERY
#   If the script fails after `git push` but before `gh release create`,
#   you will have a pushed tag with no GitHub release. Fix manually:
#
#       gh release create <VERSION> --generate-notes --title "<VERSION>"
#
#   If the script fails after committing but before pushing, reset locally:
#
#       git tag -d <VERSION>
#       git reset --hard HEAD~1
#
# PORTABILITY
#   Requires GNU sed (for -i without backup suffix). On macOS, install
#   gnu-sed (`brew install gnu-sed`) and ensure it shadows BSD sed, or
#   run inside a Linux container.
set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() {
    echo "create-release.sh: error: $*" >&2
    exit 1
}

info() {
    echo "create-release.sh: $*" >&2
}

usage() {
    cat <<EOF
Usage: $0 <VERSION> [--dry-run]

  VERSION   The release tag, e.g. v0.4.1 (must start with 'v')
  --dry-run Perform local stamping/commit/tag without push or GitHub release

Examples:
  $0 v0.4.1
  $0 v0.5.0 --dry-run
EOF
    exit 1
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

[[ $# -lt 1 ]] && usage

VERSION="$1"
DRY_RUN=false

if [[ $# -ge 2 && "$2" == "--dry-run" ]]; then
    DRY_RUN=true
fi

# Validate version format: must start with 'v'
[[ "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-.*)?$ ]] || \
    die "VERSION must match vX.Y.Z (e.g. v0.4.1), got: $VERSION"

# ---------------------------------------------------------------------------
# Precondition checks
# ---------------------------------------------------------------------------

# 1. gh CLI installed and authenticated
command -v gh >/dev/null 2>&1 || die "GitHub CLI (gh) is not installed"
gh auth status >/dev/null 2>&1 || die "GitHub CLI is not authenticated (run: gh auth login)"

# 2. We are in the repo root
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$ROOT"

[[ -f go.mod ]] || die "go.mod not found — run from the repo root"
grep -q 'module github.com/neumachen/gojira' go.mod || \
    die "go.mod does not declare module github.com/neumachen/gojira"

# 3. On main branch
BRANCH=$(git rev-parse --abbrev-ref HEAD)
[[ "$BRANCH" == "main" ]] || die "must be on 'main' branch, currently on: $BRANCH"

# 4. Working tree is clean
[[ -z "$(git status --porcelain)" ]] || die "working tree is not clean (commit or stash changes first)"

# 5. Tag does not exist locally
git rev-parse "$VERSION" >/dev/null 2>&1 && die "tag $VERSION already exists locally"

# 6. Tag does not exist on remote
git ls-remote --tags origin "$VERSION" | grep -q "$VERSION" && \
    die "tag $VERSION already exists on remote"

# 7. set_version.sh exists and is executable
SET_VERSION_SCRIPT="$ROOT/scripts/set_version.sh"
[[ -x "$SET_VERSION_SCRIPT" ]] || die "scripts/set_version.sh not found or not executable"

BUILDINFO_FILE="$ROOT/internal/buildinfo/buildinfo.go"
[[ -f "$BUILDINFO_FILE" ]] || die "internal/buildinfo/buildinfo.go not found"

# ---------------------------------------------------------------------------
# Stamping
# ---------------------------------------------------------------------------

info "stamping commit SHA via set_version.sh..."
"$SET_VERSION_SCRIPT"

info "setting ref to $VERSION..."
# Use the same sed pattern as set_version.sh (pipe delimiter, POSIX-extended)
sed -i -E "s|(ref[[:space:]]*=[[:space:]]*\")[^\"]*(\")|\1${VERSION}\2|" "$BUILDINFO_FILE"

# Verify the stamp
STAMPED_REF=$(grep -E '^const ref' "$BUILDINFO_FILE" | sed -E 's/.*"([^"]*)".*/\1/')
[[ "$STAMPED_REF" == "$VERSION" ]] || die "stamping failed: ref is '$STAMPED_REF', expected '$VERSION'"

info "stamped buildinfo.go: ref=$VERSION"

# ---------------------------------------------------------------------------
# Git operations
# ---------------------------------------------------------------------------

info "committing stamped buildinfo.go..."
git add "$BUILDINFO_FILE"
git commit -m "chore(release): stamp ${VERSION}"

info "tagging $VERSION..."
git tag "$VERSION"

if $DRY_RUN; then
    info "DRY-RUN: skipping push and GitHub release creation"
    info ""
    info "To inspect the result:"
    info "  git log -1"
    info "  cat internal/buildinfo/buildinfo.go | grep -E '^const'"
    info ""
    info "To undo the dry-run:"
    info "  git tag -d $VERSION"
    info "  git reset --hard HEAD~1"
    exit 0
fi

info "pushing to origin (main + tag)..."
git push origin main --follow-tags

# ---------------------------------------------------------------------------
# GitHub release
# ---------------------------------------------------------------------------

info "creating GitHub release $VERSION..."
RELEASE_URL=$(gh release create "$VERSION" --generate-notes --title "$VERSION" 2>&1 | tail -1)

info ""
info "=========================================="
info "Release $VERSION created successfully!"
info "=========================================="
info ""
info "Release URL: $RELEASE_URL"
info ""
info "Verify with:"
info "  go install github.com/neumachen/gojira/cmd/gojira@${VERSION}"
info "  gojira --version"
info ""
info "Expected output: gojira $VERSION"
