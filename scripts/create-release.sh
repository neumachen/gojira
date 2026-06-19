#!/usr/bin/env bash
# create-release.sh — create a gojira release via GitHub CLI.
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
#   2. Creates a GitHub release with the specified tag
#   3. GitHub CLI auto-creates the tag on the current HEAD
#   4. CD workflow triggers and builds Docker images with stamped version
#
# VERSION RESOLUTION
#   gojira uses runtime/debug.ReadBuildInfo() to detect the module version
#   at runtime. When installed via `go install ...@vX.Y.Z`, the binary
#   automatically reports vX.Y.Z — no source stamping needed.
#
#   CD additionally stamps the Docker build context via scripts/set_version.sh
#   for container images.
#
# DRY-RUN MODE
#   Pass --dry-run as the second argument to validate preconditions and
#   show what would be done without actually creating the release.
#
# PREREQUISITES
#   - GitHub CLI (gh) installed and authenticated
#   - On main branch with clean working tree
#   - Tag must not already exist
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
  --dry-run Validate preconditions without creating the release

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

# ---------------------------------------------------------------------------
# Show what will happen
# ---------------------------------------------------------------------------

CURRENT_SHA=$(git rev-parse --short HEAD)
info ""
info "Ready to create release:"
info "  Tag:     $VERSION"
info "  Commit:  $CURRENT_SHA ($(git log -1 --format='%s' HEAD))"
info "  Branch:  $BRANCH"
info ""

if $DRY_RUN; then
    info "DRY-RUN: would execute:"
    info "  gh release create \"$VERSION\" --generate-notes --title \"$VERSION\""
    info ""
    info "After release, verify with:"
    info "  go install github.com/neumachen/gojira/cmd/gojira@${VERSION}"
    info "  gojira --version"
    info ""
    info "Expected output: gojira $VERSION"
    exit 0
fi

# ---------------------------------------------------------------------------
# Create the release
# ---------------------------------------------------------------------------

info "Creating GitHub release $VERSION..."
gh release create "$VERSION" --generate-notes --title "$VERSION"

info ""
info "=========================================="
info "Release $VERSION created successfully!"
info "=========================================="
info ""
info "The tag was created on commit $CURRENT_SHA."
info "CD workflow will build and push Docker images."
info ""
info "Fetch the new tag locally:"
info "  git fetch --tags origin"
info ""
info "Verify the release:"
info "  go install github.com/neumachen/gojira/cmd/gojira@${VERSION}"
info "  gojira --version"
info ""
info "Expected output: gojira $VERSION"
