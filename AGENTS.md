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

### Write operations

The gRPC service now exposes write operations (CreateIssue, UpdateIssue,
AddComment, ListTransitions, TransitionIssue) in addition to the read RPCs.
Writes use the server's single-tenant identity, support a dry-run preview for
create/update, and surface Jira field-level validation errors. Issue deletion
is intentionally unsupported. Per AGENTS guardrails, treat write operations as
mutating actions.

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
