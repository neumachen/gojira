# AGENTS.md — gojira

## Project purpose

gojira is a Go package/application for recursively mirroring Jira issue graphs into Markdown.

The intended workflow is:

1. Start from one or more Jira issue keys, such as `PLATENG-1147`.
2. Fetch the issue using official/public Atlassian APIs.
3. Fetch or use current Atlassian API/schema information where appropriate.
4. Parse Jira issue body content, rich text, relationships, remote links, and development metadata.
5. Recursively follow Jira links.
6. Recognize GitHub pull request links as pull request references even when no Jira key is available.
7. Preserve non-Jira links as standard Markdown links without recursively crawling them by default.
8. Write each downloaded Jira issue to `<ISSUE-KEY>/index.md`.
9. Represent per-issue outbound references in `<ISSUE-KEY>/references/` or a clearly justified equivalent.

## Expected architecture

Prefer a library-first Go design with an optional CLI.

Likely package responsibilities:

- Jira API client and transport/auth handling.
- Jira issue fetching and pagination.
- Schema and metadata loading/caching.
- Atlassian Document Format traversal.
- Link extraction and classification.
- GitHub pull request URL recognition.
- Crawl coordination, queue management, deduplication, and visited-set handling.
- Relationship normalization.
- Markdown rendering.
- Filesystem storage.
- CLI/configuration.

Keep the code modular, but do not over-abstract. Add interfaces only when they make testing or integration boundaries clearer.

## Jira/API guardrails

- Assume Jira Cloud unless the user or repository context says otherwise.
- Prefer Jira Cloud REST API v3 for platform operations unless official docs indicate a better current API for a specific feature.
- Use official/public Atlassian APIs only unless the user explicitly approves otherwise.
- Do not invent endpoints, fields, or response shapes.
- Distinguish:
  - Jira REST OpenAPI schema,
  - Atlassian Document Format schema,
  - tenant-specific Jira field metadata,
  - raw issue JSON.
- Preserve unknown/custom fields safely.
- Handle pagination, permissions, rate limits, network failures, and partial crawl results.
- Treat Jira UI Development-panel data as potentially separate from normal issue REST responses.
- Do not rely on PR titles, branch names, commit messages, or PR metadata containing Jira keys.

## Markdown output rules

Canonical issue path:

```text
<ISSUE-KEY>/index.md
```

Example:

```text
PLATENG-1147/index.md
```

Per-issue references directory:

```text
<ISSUE-KEY>/references/
```

Jira links:

- Recursively crawl by default, subject to configured limits.
- Link to the downloaded issue's canonical Markdown page when available.
- Mark unresolved/unfetched Jira links clearly.

Non-Jira links:

- Preserve as standard Markdown links.
- Do not recursively crawl by default.

GitHub pull request links:

- Recognize and label as pull request references.
- Do not require Jira keys.
- Preserve the URL even if metadata is incomplete.

## Go coding standards

- Use idiomatic Go.
- Keep packages small and cohesive.
- Use `context.Context` for external operations.
- Make HTTP clients injectable for tests.
- Return errors with actionable context.
- Prefer table-driven tests.
- Use fixtures and golden files where helpful.
- Avoid live Jira/GitHub calls in unit tests.
- Place integration / end-to-end tests (those exercising the public facade across packages, using `httptest` fakes and `testdata` fixtures) in the dedicated `integtest` package at the module root — never in the root package. Keep package-local unit tests (white-box `package x`, or `package x_test` colocated with the code they cover) next to their source.
- Run `gofmt` on changed Go files.
- Avoid unrelated rewrites or drive-by cleanup.

## Test commands

Primary checks are configured through Aider:

```bash
./scripts/aider-lint-go.sh
./scripts/aider-test.sh
```

Expected default behavior:

- `scripts/aider-lint-go.sh` formats passed Go files with `gofmt` and runs `go vet ./...` when `go.mod` exists.
- `scripts/aider-test.sh` runs `go test ./...` when `go.mod` exists.

If these scripts are not executable, run:

```bash
chmod +x scripts/aider-lint-go.sh scripts/aider-test.sh
```

### Proto codegen

The gRPC contract lives in `proto/gojira/v1/gojira.proto`; generated Go
code is committed under `gen/gojira/v1/`. After editing the proto, run:

```bash
./scripts/gen-proto.sh
```

This runs `buf lint` then `buf generate`. Commit the regenerated
`*.pb.go` and `*_grpc.pb.go` files alongside the proto change.

### Package layout

Packages are organized by importability:

- `pkg/` holds the reusable, third-party-importable packages
  (`pkg/classify`, `pkg/client`, `pkg/log`).
- The repo root holds the library facade (`gojira.go`, `format.go`,
  package `gojira`, imported as `github.com/neumachen/gojira`) — the
  module's public front door.
- `internal/` holds everything not meant for external import: the
  protocol services (`internal/grpc`, `internal/mcp`), the CLI wiring
  (`internal/cli`), and the domain machinery (crawl, fetch, parse,
  render, config, …).
- `cmd/` holds the executables. `cmd/gojira/main.go` is a thin
  entrypoint that calls `internal/cli.Run`.
- `gen/` holds generated protobuf code.

Both protocol services expose a single encapsulated entry point —
`grpc.Serve(ctx, cfg)` and `mcp.Serve(ctx, cfg)` — so the binary only
ever passes a fully-resolved Config; the SDKs never leak into cmd/.

### Test layout

Integration and end-to-end tests live in the `integtest` package at the
module root (`integtest/`), together with their `testdata/` fixtures.
This keeps the module-root package (`github.com/neumachen/gojira`)
production-only and gives cross-package acceptance tests a single home.

- New integration/E2E tests (those that import the public `gojira`
  facade plus the `pkg/client` and `pkg/classify` packages and drive
  them through `httptest` servers or fixture files) MUST be added
  under `integtest/`, in `package integtest`.
- Fixtures consumed by those tests live under `integtest/testdata/`
  and are read with paths relative to the package directory (e.g.
  `filepath.Join("testdata", "acceptance", name)`).
- Package-local unit tests stay with the code they test (e.g.
  `internal/crawl/crawl_test.go`, `pkg/client/*_test.go`). Do not
  move those into `integtest`.

### Write operations

The gRPC service now exposes write operations (CreateIssue, UpdateIssue,
AddComment, ListTransitions, TransitionIssue) in addition to the read RPCs.
Writes use the server's single-tenant identity, support a dry-run preview for
create/update, and surface Jira field-level validation errors. Issue deletion
is intentionally unsupported. Per AGENTS guardrails, treat write operations as
mutating actions.

### MCP server

`gojira mcp` runs a Model Context Protocol stdio server so AI hosts can
use gojira's capabilities as MCP tools. It is config-driven dual-mode
via the required `mcp.mode`: `self` runs the gojira facade in-process,
`bridge` forwards every tool call to a running `gojira serve` gRPC
server at `server.address`. The mutating tools (`create_issue`,
`update_issue`, `add_comment`, `transition_issue`) are absent from
`tools/list` unless `mcp.allow_writes: true` is set; the read tools
(`classify`, `get_issue`, `crawl`, `get_graph`, `list_transitions`) are
always present. The official Go SDK
`github.com/modelcontextprotocol/go-sdk` is the new dependency that
powers this surface.

**Stdout-purity invariant (load-bearing).** In `gojira mcp`, stdout
carries the MCP JSON-RPC protocol stream — nothing else may be written
there. Every diagnostic, log line, or error message on the mcp serving
path MUST go to stderr (see `internal/cli/mcp.go` for the cmd action
and `internal/mcp/serve.go` for the encapsulated `Serve` that enforces
stdout-purity). Any new code on the MCP path must respect this
invariant; the cmd-level stdout-purity test in
`internal/cli/mcp_test.go` is the regression guard.

### Observability and tracing

`gojira crawl` supports a five-level log ladder (`error`/`warn`/`info`/
`debug`/`trace`) with intent-based meanings: `info` carries operationally
significant facts and all measurement data, `debug` carries durable
diagnostics, and `trace` is traceability (span lifecycles, raw payloads,
fan-out lineage). Use `--log-level trace --log-format json` for filterable
machine output. Authorization headers and any token are absolutely never
logged, even at trace. The crawl emits a `crawl.measurement` INFO line at
end of run with per-phase wall-clock attribution.

When implementing or testing observability-adjacent code: never log a token
or credential — the redaction is a hard rule; raw response bodies are safe.

## AiderDesk workflow

Use small, focused implementation slices.

Recommended commands:

- `/jira/plan "<slice>"` — inspect and plan without editing.
- `/jira/implement-slice "<slice>"` — implement a small tested slice.
- `/jira/review-diff` — review current uncommitted changes.
- `/jira/test` — run and summarize project checks.

Prompt files are stored in:

```text
docs/prompts/
```

Project rules are stored in:

```text
.aider-desk/rules/
```

Agent-specific rules are stored in:

```text
.aider-desk/agents/<profile>/rules/
```

## Ask before doing these things

Ask the user before:

- Adding new third-party dependencies.
- Making broad architectural changes.
- Using undocumented Jira browser/internal APIs.
- Making live Jira/GitHub calls.
- Changing authentication strategy.
- Changing the canonical Markdown output layout.
- Running destructive shell commands.
- Committing changes.

## Security and privacy

Never commit:

- Jira API tokens.
- GitHub tokens.
- Cookies.
- Private cloud IDs.
- Private Jira/GitHub URLs unless already present in committed safe test fixtures.
- Credentials or local `.env` files.

Use environment variables or ignored local config for secrets.

## Final response expectations for implementation agents

After making changes, respond with:

1. Summary of changes.
2. Files changed.
3. Tests/checks run.
4. Remaining risks/open questions.
5. Recommended next slice.
