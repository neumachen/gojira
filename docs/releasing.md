# Releasing gojira

How gojira reports its build identity and how a maintainer cuts a
release. This document is the source of truth for the version mechanism
in the repository.

## Overview

gojira reports its build identity through four functions on the root
facade:

- `gojira.Revision()` — the git SHA the binary was built from.
- `gojira.Version()` — the human-facing version string (e.g. `v0.4.1`).
  This is what `gojira --version` prints and what the MCP server
  reports as its version.
- `gojira.FullVersion()` — image-reference style: `"<version>@<revision>"`
  when both are available (e.g. `v0.4.1@abc1234`), or just `"<version>"`.
- `gojira.UserAgent()` — `"gojira/" + Version()`, sent as the
  outbound HTTP User-Agent by `pkg/client`.

## How version resolution works

gojira uses **two mechanisms** to determine its version at runtime:

1. **`runtime/debug.ReadBuildInfo()`** — Go automatically embeds module
   version and VCS metadata when you run `go install ...@vX.Y.Z`. This
   is the primary mechanism for end users.

2. **CD stamping** — For Docker images, the CD workflow runs
   `scripts/set_version.sh` to rewrite constants in the build context
   before building. This is transient and never committed.

### Resolution priority

The `Version()` function checks in order:

1. CD-stamped `ref` constant (if not empty)
2. `info.Main.Version` from `debug.ReadBuildInfo()` (if not `(devel)`)
3. CD-stamped `commit` constant (if not `dev`)
4. Fallback: `"dev"`

The `Revision()` function checks in order:

1. CD-stamped `commit` constant (if not `dev`)
2. `vcs.revision` setting from `debug.ReadBuildInfo()`
3. Fallback: `"dev"`

## Where the constants live

The CD-stampable constants live in `internal/buildinfo/buildinfo.go`:

```go
const commit = "dev"  // git SHA; CD stamps this transiently
const ref    = ""     // tag/branch; CD stamps this transiently
```

**These are always `"dev"` and `""` in committed source.** They are
only rewritten transiently by CD in the Docker build context.

## Cutting a release

Releases are simple — just create a GitHub release with a tag:

### Using the script (recommended)

```bash
# Make sure you are on main with a clean tree
git switch main
git pull --ff-only

# Create the release
./scripts/create-release.sh v0.4.1
```

The script:
1. Validates preconditions (gh authenticated, on main, clean tree, tag unused)
2. Creates a GitHub release via `gh release create`
3. GitHub CLI auto-creates the tag on the current HEAD

Pass `--dry-run` to validate without creating:

```bash
./scripts/create-release.sh v0.4.1 --dry-run
```

### Manual alternative

```bash
gh release create v0.4.1 --generate-notes --title "v0.4.1"
```

Then fetch the tag locally:

```bash
git fetch --tags origin
```

## After releasing

Once the release is created:

- **`go install ...@v0.4.1`** will report `v0.4.1` automatically
  (via `debug.ReadBuildInfo()`)
- **CD workflow** triggers and builds Docker images with stamped version
- **GHCR images** will report `v0.4.1` (via CD stamping)

## Container images (CD)

The CD workflow at `.github/workflows/cd.yml` runs
`./scripts/set_version.sh` inside the build-push job after a
full-history checkout (`fetch-depth: 0`) so the published GHCR
image reports the correct build identity.

This stamping is **transient**: the rewritten file lives only
inside the build container and is never committed. CD only runs on
pushes to `main` and to `v*` tags.

## Behavior reference

What the four accessors report for each kind of build:

| Build | `Revision()` | `Version()` | `FullVersion()` | `UserAgent()` |
| --- | --- | --- | --- | --- |
| local `go build` | `dev` | `dev` | `dev` | `gojira/dev` |
| `go install ...@v0.4.1` | `<sha>` | `v0.4.1` | `v0.4.1@<sha>` | `gojira/v0.4.1` |
| GHCR image (tag build) | `<sha>` | `v0.4.1` | `v0.4.1@<sha>` | `gojira/v0.4.1` |
| GHCR image (main build) | `<sha>` | `main` | `main@<sha>` | `gojira/main` |

## FAQ

### Why use `debug.ReadBuildInfo()` instead of stamping source?

It's simpler and cleaner:

- No source modifications needed for releases
- `go install ...@vX` automatically knows the version
- Source always has clean `"dev"` / `""` values
- CD stamping is still available for Docker images

### Why keep CD stamping if ReadBuildInfo works?

Docker builds don't go through `go install`. The CD workflow builds
from source, so it stamps the constants transiently to embed the
version in the resulting image.

### Why is `Version` a function and not a `const`?

It needs to check multiple sources at runtime (`debug.ReadBuildInfo()`,
stamped constants, fallback). A const cannot do runtime logic.
