#!/usr/bin/env bash
# set_version.sh — stamp git revision and version ref into committed source.
#
# WHAT THIS SCRIPT DOES
#   Rewrites the two unexported consts in internal/buildinfo/buildinfo.go:
#
#       const commit = "<git-sha>"
#       const ref    = "<tag-or-branch>"
#
#   so that a downstream `go install github.com/neumachen/gojira/cmd/gojira@vX`
#   compiles a binary whose buildinfo.Revision() / Version() / FullVersion()
#   report the release identity. No -ldflags trickery is involved — the
#   stamped values live in committed source.
#
# IT MUTATES COMMITTED SOURCE
#   This script edits a tracked file in-place. It is not a build-time-only
#   stamper. The intended release flow is:
#
#       1. scripts/set_version.sh           # rewrite buildinfo.go
#       2. git add internal/buildinfo/buildinfo.go && git commit
#       3. git tag vX.Y.Z                   # tag the stamping commit
#       4. git push --follow-tags           # publish
#
#   After that, `go install ...@vX.Y.Z` will produce a binary that reports
#   vX.Y.Z. The script is invoked by CD on main/tag pushes only — never on
#   PRs or regular CI runs, since mutating committed source mid-PR would
#   create noisy diffs and circular commits.
#
# PORTABILITY NOTE
#   `sed -i` semantics differ between GNU sed (Linux) and BSD sed (macOS).
#   This script targets GNU sed as used on ubuntu-latest CD runners. The
#   `-E` flag is POSIX-extended (portable across both seds), but the in-place
#   `-i` with no backup-suffix argument is GNU-only. If invoked on macOS for
#   local testing, install gnu-sed (`brew install gnu-sed`) and shim `sed`,
#   or run inside a Linux container.
set -eu

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
TARGET="$ROOT/internal/buildinfo/buildinfo.go"

if [ ! -f "$TARGET" ]; then
    echo "set_version.sh: target file not found: $TARGET" >&2
    exit 1
fi

GIT_SHA=$(git rev-parse --verify HEAD)
GIT_TAG=$(git describe --tags --exact-match 2>/dev/null || echo)
GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo)
REF="${GIT_TAG:-$GIT_BRANCH}"

# Use `|` as the sed delimiter because the replacement values are:
#   - GIT_SHA: hex only, cannot contain `|`
#   - REF:     tag or branch; branches may legally contain `/`, which is why
#              we avoid `/` as the delimiter, but `|` is not a valid char in
#              git ref names (see git-check-ref-format(1)), so it is safe.
# The regex tolerates one-or-more spaces around `=` because gofmt aligns
# adjacent single-line consts.
sed -i -E "s|(commit[[:space:]]*=[[:space:]]*\")[^\"]*(\")|\1${GIT_SHA}\2|" "$TARGET"
sed -i -E "s|(ref[[:space:]]*=[[:space:]]*\")[^\"]*(\")|\1${REF}\2|" "$TARGET"

>&2 echo "set_version.sh: stamped commit=$GIT_SHA ref=$REF into $TARGET"
