# Releasing gojira

How gojira stamps its build identity and how a maintainer cuts a
release. This document is the source of truth for the version-stamping
mechanism in the repository.

## Overview

gojira reports its build identity through four functions on the root
facade:

- `gojira.Revision()` — the git SHA the binary was built from
  (`"dev"` when un-stamped).
- `gojira.Version()` — the human-facing version: the release ref
  (tag or branch) when stamped, falling back to the commit SHA
  otherwise. This is what `gojira --version` prints and what the
  MCP server reports as its version.
- `gojira.FullVersion()` — image-reference style: `"<ref>@<commit>"`
  when stamped (e.g. `v0.4.0@abc1234`), or just `"<commit>"` (e.g.
  `dev`) when un-stamped.
- `gojira.UserAgent()` — `"gojira/" + Version()`, sent as the
  outbound HTTP User-Agent by `pkg/client` (overridable per-client
  via `client.WithUserAgent`).

These values come from **committed source**. They are not injected
with `-ldflags -X` at build time, because `go install
github.com/neumachen/gojira/cmd/gojira@vX.Y.Z` compiles whatever is
at that tag with no linker flags, no Docker, and no scripts. For
`go install ...@vX.Y.Z` to print `vX.Y.Z`, the commit the tag points
at must already carry the stamped values in its source tree.

## Where the version lives

The values live in the leaf package `internal/buildinfo`, in
[`internal/buildinfo/buildinfo.go`](../internal/buildinfo/buildinfo.go),
as two unexported consts:

```go
const commit = "dev" // git SHA; rewritten at release time
const ref    = ""    // tag or branch; rewritten at release time
```

The root facade re-exports the four accessor functions from
[`version.go`](../version.go). External callers MUST use those
accessors — `internal/buildinfo` is not part of the public API and
exists only so release tooling has a single, narrow rewrite target.

`internal/buildinfo` imports only the standard library (and in
practice nothing at all), which guarantees no import cycle can be
introduced through it.

## The stamping script

[`scripts/set_version.sh`](../scripts/set_version.sh) rewrites the
two consts in place:

- `commit` ← `git rev-parse --verify HEAD` (always).
- `ref` ← `git describe --tags --exact-match` when HEAD is exactly
  on a tag, otherwise `git rev-parse --abbrev-ref HEAD` (the
  current branch name).

The script resolves its own repo root, so it works from any CWD.
It writes its summary line to stderr only and mutates the file in
place — there is no flag to dry-run. It is bash (`#!/usr/bin/env
bash`, `set -eu`) and uses GNU `sed -i`; on macOS, run it inside a
Linux container or shim `gnu-sed`.

The script does NOT run `git add`, `git commit`, or `git tag`. The
maintainer drives those steps.

## Cutting a release

The release flow has one invariant: **the tag must end up on a
commit whose `internal/buildinfo/buildinfo.go` literally contains
the release version string in `ref`.** Any sequence that achieves
that is correct.

### Automated (recommended)

Use [`scripts/create-release.sh`](../scripts/create-release.sh) to
automate the entire flow via the GitHub CLI:

```bash
# Make sure you are on main with a clean tree
git switch main
git pull --ff-only

# Cut the release (stamps, commits, tags, pushes, creates GH release)
./scripts/create-release.sh v0.4.1
```

The script:
1. Validates preconditions (gh authenticated, on main, clean tree, tag unused)
2. Runs `set_version.sh` to stamp the commit SHA
3. Edits `ref` to the target version
4. Commits, tags, and pushes
5. Creates a GitHub release with auto-generated notes

Pass `--dry-run` as a second argument to perform local
stamping/commit/tag without pushing or creating the release:

```bash
./scripts/create-release.sh v0.4.1 --dry-run
# Inspect the result, then undo:
git tag -d v0.4.1
git reset --hard HEAD~1
```

### Manual recipe

If you prefer to drive each step yourself:

```bash
# 1. Decide the version (example: v0.4.0). Make sure you are on the
#    commit you want to release and the working tree is clean.
git switch main
git pull --ff-only

# 2. Stamp the commit SHA and edit `ref` to the intended tag.
#    Run the script first to fill in `commit`, then hand-edit `ref`
#    to "v0.4.0" in internal/buildinfo/buildinfo.go.
./scripts/set_version.sh
$EDITOR internal/buildinfo/buildinfo.go   # set: const ref = "v0.4.0"

# 3. Commit the stamped file.
git add internal/buildinfo/buildinfo.go
git commit -m "chore(release): stamp v0.4.0"

# 4. Tag that exact commit and publish.
git tag v0.4.0
git push --follow-tags

# 5. Create the GitHub release.
gh release create v0.4.0 --generate-notes --title "v0.4.0"
```

After step 5, `go install github.com/neumachen/gojira/cmd/gojira@v0.4.0`
produces a binary that reports `v0.4.0`.

### Why the manual `ref` edit (in the manual recipe)

When `scripts/set_version.sh` runs **before** the tag exists,
`git describe --tags --exact-match` does not see the new tag yet,
so the script stamps `ref` with the current branch name (e.g.
`main`) instead of the version. Hand-editing `ref` to the intended
tag in the same release commit avoids the ordering puzzle entirely
and keeps the recipe linear: stamp → edit → commit → tag.

The automated `create-release.sh` handles this by running
`set_version.sh` first, then using `sed` to overwrite `ref` with
the target version before committing.

## Container images (CD)

The CD workflow at
[`.github/workflows/cd.yml`](../.github/workflows/cd.yml) runs
`./scripts/set_version.sh` inside the build-push job after a
full-history checkout (`fetch-depth: 0`) so the published GHCR
image reports the correct build identity.

This stamping is **transient**: the rewritten file lives only
inside the build container and is never committed. CD only runs on
pushes to `main` and to `v*` tags — never on PRs or regular CI —
because mutating a tracked file mid-PR would create noisy diffs
and circular commits.

The Docker build no longer passes any `-ldflags -X
github.com/neumachen/gojira.Version=...` override. That mechanism
was removed in v0.4 because `gojira.Version` is now a function, and
`-X` can only set string variables, not function bodies.

## Behavior reference

What the four accessors report for each kind of build:

| Build | `Revision()` | `Version()` | `FullVersion()` | `UserAgent()` |
| --- | --- | --- | --- | --- |
| local `go build` (un-stamped) | `dev` | `dev` | `dev` | `gojira/dev` |
| `go install ...@main` (after a stamped `main` commit) | `<sha>` | `main` | `main@<sha>` | `gojira/main` |
| `go install ...@v0.4.0` (release) | `<sha>` | `v0.4.0` | `v0.4.0@<sha>` | `gojira/v0.4.0` |
| GHCR image built by CD on `main` | `<sha>` | `main` | `main@<sha>` | `gojira/main` |
| GHCR image built by CD on tag `v0.4.0` | `<sha>` | `v0.4.0` | `v0.4.0@<sha>` | `gojira/v0.4.0` |

## FAQ

### Why is the version in committed source instead of `-ldflags`?

`go install github.com/neumachen/gojira/cmd/gojira@vX.Y.Z` is the
primary install path for a Go module. It compiles the code at the
tag with no linker flags, no build script, and no Dockerfile. The
only way to make that command produce a binary that knows its own
version is for the version to be present in the source tree at the
tagged commit. Hence: stamp, commit, then tag.

### Why is `Version` a function and not a `const`?

Up to and including v0.3, the facade exposed
`const Version = "v0.3.0"`. That constant drifted from reality
whenever a release was cut without bumping it. Making `Version` a
function lets release tooling rewrite the underlying consts in
`internal/buildinfo` without touching the public facade, and lets
the same accessor cover both un-stamped (`dev`) and stamped builds.

The cost is a small API break: callers must move from
`gojira.Version` to `gojira.Version()`.

### Why are `commit` and `ref` unexported?

They are a rewrite target for `scripts/set_version.sh`, not part
of the public API. The accessors (`Revision`, `Version`,
`FullVersion`, `UserAgent`) are the contract. Keeping the consts
unexported lets the stamping mechanism evolve (e.g. switch from
consts to a generated file, or read from `debug.ReadBuildInfo()`)
without breaking external callers.

### Why does CD re-stamp if releases are already stamped in source?

CD also publishes images built from `main` between releases. Those
commits do not carry a release tag in `ref`, so the in-container
re-stamp fills in the branch name and the current SHA. For tagged
releases, the re-stamp is idempotent: the committed source already
contains the same values, so the rewrite is a no-op in effect.
