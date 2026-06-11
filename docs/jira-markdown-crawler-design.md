# gojira — Design Decisions (mini-doc)

**Scope of this document:** package boundaries, dependency direction,
and testability seams for the gojira v0.1 MVP.

**Out of scope:** interface signatures, struct field names, concrete
types, error taxonomy, concurrency model details, HTTP retry policy
specifics. Those are decided in implementation tasks.

**Companion documents:**

- Product requirements: `.aider-desk/shiki/outputs/gojira-prd/full-prd.md`
- Mission and rules: `.aider-desk/rules/`, `AGENTS.md`
- Workflow guidance: `docs/aiderdesk-workflow.md`

**License:** MIT (see PRD §1).

---

## 1. Purpose

The PRD commits to *what* gojira does and to a clean library/CLI split.
It deliberately does not specify package decomposition. This document
fills exactly that gap: it locks the top-level package layout, the
dependency direction between packages, and the testability seam each
package is built around.

Once these are committed, `shiki-plan` can generate implementation
tasks that each target a single package with a clear input/output
contract and isolated tests.

---

## 2. Guiding principles

1. **Single responsibility per package.** Each internal package answers
   one question: "what bytes do I get from Jira?", "what does this JSON
   mean?", "what links live inside this issue?", "what Markdown
   represents it?", "where does the output file go?", etc. If a package
   would answer two of those, it is two packages.
2. **Compiler-enforced separation.** Responsibilities are split across
   sibling packages, not files within one package. The Go compiler then
   prevents accidental leakage: a pure parser cannot acquire a network
   dependency without a visible import that fails review.
3. **Acyclic dependency direction.** Imports flow one way, from
   composition layers down to leaves. Leaves know nothing about who
   composes them.
4. **Public surface is small and intentional.** Only packages explicitly
   designed for third-party use are top-level; everything else lives
   under `internal/` and is unimportable outside the module.
5. **Pure logic at the bottom, I/O at the edges.** Parsers,
   classifiers, and renderers are pure. Network and filesystem live in
   dedicated leaf packages with injectable seams.
6. **No package logs directly.** All observable events flow through a
   single sink interface; the caller decides how to format and route
   them. The CLI is one such caller.
7. **Signatures must be honest.** Every parameter that influences a
   function's behavior at runtime is declared in its signature.
   Constants embedded in the body that *could* meaningfully differ
   between calls are not allowed; the signature would lie about the
   contract. See `docs/engineering-principles.md` for the rule
   statement and concrete examples this project has caught.

See also: `docs/engineering-principles.md` for the broader set of
human-facing engineering rules. `.aider-desk/rules/10-go-engineering.md`
carries the same rules for AI agents; the two are kept in sync.

---

## 3. Package layout

```
gojira/
├── gojira.go                       facade: the PRD library capabilities
├── client/                  PUBLIC HTTP transport, auth, retries
├── classify/                PUBLIC pure URL / issue-key classification
├── log/                     PUBLIC slog-handler defaults (text and JSON)
├── cmd/
│   └── gojira/                     CLI binary (thin consumer)
└── internal/
    ├── fetch/                      uses client; returns raw issue bytes for a key
    ├── parse/                      pure; raw issue bytes → typed Issue value
    ├── extract/                    pure; Issue → outbound references
    ├── adf/                        ADF traversal; used by extract and render
    ├── crawl/                      composes fetch+parse+extract+hierarchy+devstatus; concurrency, dedup, termination
    ├── hierarchy/                   JQL-based hierarchy child discovery (parent + Epic Link)
    ├── devstatus/                   Jira Dev Status API enrichment (pull-request URLs)
    ├── render/                     pure; Issue → Markdown
    ├── output/                     filesystem writer; skip-if-exists, idempotency
    ├── events/                     single sink interface for structured events
    └── config/                     validation + construction of the runtime config
```

### 3.1 Public packages (importable by third parties)

| Package | Why public |
|---|---|
| `gojira` (facade) | The four PRD-named capabilities live here so a third-party Go program can `import "github.com/neumachen/gojira"` and access everything it needs. Surface is intentionally small. |
| `client` | A correctly-implemented Jira Cloud HTTP client (auth, pagination, rate-limit, retries) is independently useful. Importing only `client` lets a third-party tool issue Jira calls without pulling in gojira's crawl semantics. |
| `classify` | Pure URL / issue-key classification has obvious standalone value (a different Jira tool, a link checker, etc.). The PRD treats it as an independent capability (acceptance criterion 1). |
| `log` | The project's two default `slog.Handler` implementations (text and JSON), plus a small `New(format, level, w)` helper. Embedders may bring their own `slog.Handler`; this package exists so callers who want gojira's defaults get them in one import. Small surface; no dependency on any other gojira package. |

### 3.2 Internal packages (unimportable outside the module)

Everything else is `internal/` because it exists to implement gojira's
specific product, not to be reused on its own. Promoting any of these
to public would commit us to API stability we do not yet want.

---

## 4. Package responsibilities (one paragraph each)

**`gojira` (facade)** — Exposes the four named PRD capabilities:
`Classify`, `FetchAndRender`, `Crawl`, and `LoadConfig`. Composes the
internal packages into capability-shaped functions. Holds no business
logic of its own; orchestration logic lives in `crawl`. This is the
only place that knows the full library surface.

**`client`** (public) — Jira Cloud HTTP transport. Authenticates,
issues requests, handles pagination, handles 429 / rate-limit retry,
returns raw response bytes (or a typed lightweight response). Knows
nothing about issues, ADF, links, Markdown, or crawling. Constructed
from a config object; testable via an injectable `http.RoundTripper`.

**`classify`** (public) — Given a string and a configured Jira site,
returns one of: Jira issue key, Jira issue URL, GitHub pull request URL,
external link. Pure function; no I/O; no other internal imports. The
public seam for SRP-clean link recognition.

**`internal/fetch`** — Given an issue key and a `client`, returns the
raw issue payload bytes (or a thin typed wrapper). Knows the Jira API
shape only to the extent of "which endpoint do I call for an issue."
Does not parse, render, classify, or recurse. Single SRP: *get*.

**`internal/parse`** — Given raw issue bytes, returns a typed `Issue`
value (fields the renderer and extractor need: key, summary, status,
description ADF, relationships, remote links, custom fields, dev
metadata pointer). Pure; no network; no rendering. Preserves unknown
custom fields safely. Single SRP: *interpret*.

**`internal/extract`** — Given a parsed `Issue`, returns the set of
outbound references discovered within it (Jira keys, GitHub PR URLs,
external links), each tagged with its source (description / relationship
/ remote link / dev metadata). Delegates ADF traversal to `adf` and
link classification to `classify`. Pure; no network. Single SRP:
*discover what to follow*.

**`internal/adf`** — Atlassian Document Format traversal. Walks an ADF
document, surfaces text content, link marks, and unknown nodes
(preserved as-is, never silently dropped per the PRD). Used by both
`extract` (to find links) and `render` (to convert ADF to Markdown).
Pure; no network. Single SRP: *traverse ADF*.

**`internal/crawl`** — The orchestrator. Composes `fetch → parse →
extract → hierarchy` for each issue, manages the work queue, the
visited set, deduplication, depth/issue caps, termination, and
concurrency. The only package that knows the end-to-end recursive
workflow defined in the PRD's Project Givens. Calls `render` and
`output` for each successfully-processed issue. Emits events to
`events`.

**`internal/devstatus`** — Enriches an already-fetched issue with
pull-request URLs from Jira's Dev Status API
(`/rest/dev-status/1.0/issue/detail`). The endpoint is not in the
documented Jira Cloud platform REST API but has powered Atlassian's
own Jira UI for over a decade. The documented `customfield_10000`
development-summary field is parsed as a per-issue gate so issues
with zero PRs incur no API call. One request is issued per configured
application type (`GitHub`, `Bitbucket`, etc.); results are merged
and deduplicated by PR URL. Per-application failures are non-fatal:
partial results are returned alongside a joined error the caller can
log but ignore. Pure HTTP plus stdlib state; never imports crawl,
render, output, events, fetch, extract, adf, or classify. Single SRP:
*surface pull-request URLs not present in the per-issue GET response*.

**`internal/hierarchy`** — Discovers hierarchy children for an
already-fetched issue via JQL search. Runs `parent = "KEY"` against
the modern Jira parent field and, where the Epic Link custom field
is configured or auto-detected from the tenant field metadata,
additionally runs `"Epic Link" = "KEY"`. Returns the deduplicated,
sorted set of child keys. Caches the Epic Link field ID via
`sync.Once` so all crawl workers share a single auto-detection
lookup. Pure HTTP plus stdlib state; never imports crawl, render,
output, events, fetch, extract, adf, or classify. Single SRP:
*surface keys not present in the per-issue GET response*.

**`internal/render`** — Given a parsed `Issue` (and optionally a set of
already-fetched neighbour keys for relative links), returns the Markdown
content for `<KEY>/index.md` and for `<KEY>/references/outbound.md`.
Pure; no network; no filesystem. Entirely separate from `crawl` —
`crawl` decides *when* to render, `render` decides *what the Markdown
looks like*. Single SRP: *generate Markdown*.

**`internal/output`** — Filesystem writer. Given a path and content,
writes the file with idempotency rules (skip-if-exists vs. refetch as
configured). Owns directory creation and the per-issue layout
(`<KEY>/index.md`, `<KEY>/references/`). Knows nothing about Jira,
ADF, or Markdown content. Single SRP: *write bytes to disk*.

**`internal/events`** — The single source of truth for the sink
interface used by every other package to report progress, warnings,
errors, and partial-success states. Defines event types and the sink
contract. The library never logs directly; callers (the CLI, a
third-party program) inject an implementation. Single SRP: *event
contract*.

**`internal/config`** — Given a key-value map (from env / flags / file),
validates and constructs the runtime config object consumed by every
other package. Owns the canonical list of `GOJIRA_*` keys defined in
PRD §6. Pure; no source-specific loading (the CLI does env/flag
loading; `config` only validates and constructs). Single SRP:
*validate and shape config*.

**`cmd/gojira`** — The CLI binary. Parses flags, loads environment
variables, optionally loads a config file (post-MVP), constructs an
`events` sink that formats to stderr, calls the `gojira` facade,
handles SIGINT/SIGTERM, and translates the returned crawl summary into
an exit code per PRD §9. Adds no capability the library does not have.

---

## 5. Dependency direction

Imports flow downward only. No cycles.

```
                          cmd/gojira
                              │
                              ▼
                           gojira  (facade)
                              │
                              ▼
                     internal/crawl  (orchestrator)
              ┌──────────┬──┴──┬─────────────┬───────────┐
              ▼          ▼     ▼             ▼           ▼
       internal/fetch  parse extract     render       output
              │                │            │
              ▼                ▼            ▼
           client            adf ◄─── (also used by render)
                              │
                              ▼
                          classify
                              │
                              ▼
                          (nothing)

   internal/events  ← injected into crawl by the facade; used by every
                      package that reports observable progress.
   internal/config  ← built by the facade from CLI/caller input; passed
                      by value or by accessor interface to client,
                      fetch, crawl, output as needed.
```

### 5.1 Hard rules the layout enforces

- **`classify` imports nothing project-internal.** Pure leaf.
- **`adf` imports only `classify`.** Pure traversal that already knows
  how to label discovered link strings.
- **`parse` imports nothing project-internal.** Pure JSON → struct.
- **`client` imports nothing project-internal except `config`** (for
  site URL, credentials, timeouts). No knowledge of issues, ADF, links,
  or Markdown.
- **`fetch` imports `client` and `config`.** No knowledge of parsing,
  ADF, links, rendering, or crawling.
- **`extract` imports `parse`, `adf`, and `classify`.** Pure; never
  imports `client`, `fetch`, `crawl`, `render`, or `output`.
- **`render` imports `parse` and `adf`.** Pure; never imports `client`,
  `fetch`, `crawl`, or `output`.
- **`output` imports `config`.** Knows the layout rules; nothing else.
- **`hierarchy` imports `client`, `config`, and `parse`** (plus
  `stdlib` and `errext`). It must never import `crawl`, `render`,
  `output`, `events`, `fetch`, `extract`, `adf`, or `classify`. The
  composition direction is one-way: `crawl` depends on `hierarchy`,
  not the other way around.
- **`devstatus` imports `client`, `config`, and `parse`** (plus
  `stdlib` and `errext`). It must never import `crawl`, `render`,
  `output`, `events`, `fetch`, `extract`, `adf`, `classify`, or
  `hierarchy`. The composition direction is one-way: `crawl` depends
  on `devstatus`, not the other way around.
- **`crawl` imports `fetch`, `parse`, `extract`, `hierarchy`,
  `devstatus`, `render`, `output`, `events`, and `config`.** It is
  the only composition layer.
- **`gojira` (facade) imports `crawl`, `classify`, `client`, `config`,
  `events`.** It composes the public capability surface.
- **`log` imports nothing project-internal.** Standalone leaf
  alongside `classify`; consumers may use it without pulling in any
  crawl machinery.
- **`cmd/gojira` imports `gojira`, `config`, `events`, and `log`.** It
  does not reach into `internal/*` directly.

Any import that violates these rules is a design regression and must
be flagged in review.

### 5.2 External dependencies

The package boundaries above govern *project-internal* imports. The
following external dependencies are allowed, scoped to the packages
that need them. Adding a new external dependency requires a written
justification in PRD §1.1.

| Dependency | Allowed in | Purpose |
|---|---|---|
| `github.com/neumachen/envar` | `internal/config` | Tag-driven env-var parsing. |
| `github.com/neumachen/errorx` | every package that originates or wraps an error | Error construction with stack-trace capture. Sentinels remain `errors.New`. |
| `github.com/stretchr/testify` | every `*_test.go` file | `assert` for soft checks, `require` for hard preconditions. |
| `log/slog` (stdlib) | `log/`, `cmd/gojira`, anywhere that uses a logger | Structured logging substrate. |
| `github.com/urfave/cli/v3` | `cmd/gojira` only | CLI framework: declarative flags, subcommands, env-var value sources, help/version printing. Replaces stdlib `flag` and the hand-rolled flag-overrides-env overlay. |

---

## 6. Testability seams

| Package | Purity | Test approach |
|---|---|---|
| `classify` | Pure | Table-driven tests over string inputs and configured-site values. |
| `parse` | Pure | Golden fixtures: representative Jira issue JSON (sanitized) → expected typed `Issue` values. Covers unknown custom-field preservation. |
| `adf` | Pure | Golden ADF fixtures → expected text, link set, unknown-node markers. |
| `extract` | Pure | Constructed `Issue` values → expected outbound reference sets. No fixtures needed beyond compact in-test literals. |
| `render` | Pure | Golden tests: `Issue` value → expected Markdown content (compared byte-for-byte against `testdata/*.md`). |
| `config` | Pure | Table-driven validation tests; precedence tests live in `cmd/gojira` since the CLI owns sourcing. |
| `client` | I/O via injected `http.RoundTripper` | `httptest.Server` returning canned responses; tests cover pagination, 429 retry, auth header construction. No live Jira. |
| `fetch` | I/O via injected `client` | Faked client interface; tests verify which endpoints are called for a key. |
| `output` | Local FS only | `t.TempDir()`; assert file contents and idempotency behavior. |
| `events` | Interface | Trivial fake sink that records events; used by other packages' tests. |
| `crawl` | Composes injected interfaces | Faked `fetch`, `render`, `output`, `events`; tests dedup, depth/issue cap, termination, partial-success rollup, concurrency invariants. |
| `hierarchy` | I/O via injected `client.Client` | `httptest.Server` returning canned `/search/jql` and `/field` responses; tests verify parent + Epic Link merge, dedup, auto-detection caching, partial-failure isolation, configured override, and `HierarchyCapable` classification table. |
| `devstatus` | I/O via injected `devStatusClient` interface | Faked client interface; tests verify unconditional fan-out across every configured (application, dataType) pair, per-application fan-out merging, URL/SHA dedup, partial-failure isolation (returns partial data + joined error), and total-failure escalation. The `customfield_10000` summary blob is not parsed; the smart gate that previously did so produced two silent-miss bugs and has been removed (see §10 v9). |
| `gojira` (facade) | Composes real internals | A small set of end-to-end tests using `httptest.Server` + `t.TempDir()` for the highest-value PRD acceptance criteria. |
| `cmd/gojira` | Thin | Smoke tests for flag parsing, env-var precedence, signal handling, and exit-code mapping. |

**Iron rule:** no test in this module makes a live Jira or GitHub
network call. Live integration tests (if any) live behind a build tag
and are off by default, per `AGENTS.md` and rule `40`.

---

## 7. How the three separations you required are realized

You asked for three things to be cleanly separated; this layout makes
each a distinct, compiler-enforced boundary:

1. **The Jira client is separate.** → `client/` is public, has no
   project-internal dependencies beyond `config`, and knows nothing
   about issues, ADF, links, or Markdown. It can be imported and used
   on its own.
2. **The function for pulling issues is separate.** → `internal/fetch`
   does *only* "given a key and a client, return raw issue bytes."
   Parsing (`internal/parse`) and link discovery (`internal/extract`)
   are sibling packages, not internal helpers of `fetch`. Each of the
   three can be tested, reused, and reasoned about independently.
3. **Link reading and external-link handling are separate.** →
   `internal/adf` traverses ADF and surfaces link strings;
   `classify/` (public, pure) decides what each link string *is*;
   `internal/extract` composes the two to produce the outbound
   reference set for an `Issue`. Markdown rendering is in
   `internal/render`, which never sees the network.

---

## 8. Deployment shape

gojira ships two distribution forms:

1. **A Go binary** installable via
   `go install github.com/neumachen/gojira/cmd/gojira@latest`. This
   is the canonical artifact: a single static binary with no runtime
   dependencies beyond the standard CA store.

2. **A container image** built from the repository's `Dockerfile`.
   The image uses a two-stage build:
   - **Builder stage** (`golang:1.26.3-alpine3.23`): compiles the
     same `cmd/gojira` binary with `CGO_ENABLED=0`, `-trimpath`,
     and stripped symbols.
   - **Runtime stage** (`alpine:3.23`): contains only the binary,
     `ca-certificates` (for HTTPS to Jira Cloud), `tzdata` (for
     Jira's timezone-aware timestamps), and a fixed non-root user
     (UID/GID 65532, matching the distroless convention).

   `docker-compose.yml` is provided as convenience tooling. gojira
   is a one-shot CLI, so the compose services are designed for
   `docker compose run --rm`, not `docker compose up`. A secondary
   `gojira-shell` service exposes an interactive shell against the
   same image for debugging.

   The image deliberately does NOT include `tini` or `dumb-init`.
   The CLI handles signals via the `urfave/cli/v3` wiring and the
   crawl orchestrator's context-cancellation path; an init wrapper
   would not change observable behavior.

This section is informational, not architectural. The container
image is a packaging concern, not a design boundary: the same
binary runs identically on the host and in the container.

---

## 9. What this design defers

The following are intentionally not decided here. Each will be a
dedicated implementation task generated by `shiki-plan`.

- Interface signatures for `client`, `fetch`, `events`, and the config
  accessor types.
- The concrete shape of the `Issue` typed value produced by `parse`.
- The error taxonomy across packages (sentinel vs. typed; package of
  ownership).
- The concurrency primitive used inside `crawl` (worker pool, errgroup,
  channel pipeline) and the default concurrency value.
- HTTP retry/backoff specifics inside `client` (algorithm, max
  attempts, jitter).
- The exact Markdown templating approach inside `render` (string
  builders, `text/template`, custom mini-DSL).
- The file-locking / partial-write strategy inside `output`.
- The event taxonomy (which event kinds exist) inside `events`.
- The flag / env-var loading library inside `cmd/gojira`.

Each deferred decision is small enough to be a single implementation
task. None of them can break the boundaries above without an import
violation, which is the point.

---

## 10. Change log

| Version | Date | Notes |
|---|---|---|
| v1 | 2026-06-04 | Initial mini-doc. Locks the 12-package layout, dependency direction, and testability seams. Companion to `full-prd.md` v0.1. |
| v2 | 2026-06-05 | Added public `log/` package (slog text + JSON handlers). Added §5.2 external dependency table covering `envar`, `errorx`, `testify`, and stdlib `log/slog`. Updated §5.1 to allow `log` as a standalone leaf and to let `cmd/gojira` import `log`. Companion to `full-prd.md` v0.2. |
| v3 | 2026-06-05 | Added §8 (Deployment shape) describing the container image and `docker-compose.yml`. Defers and Change log renumber down to §9 and §10. No architectural changes. |
| v4 | 2026-06-05 | Added `internal/hierarchy` package (JQL-based child discovery). Updated §3 layout, §4 with the new package paragraph, §5.1 with the new dependency rule, and §6 testability seam row. The new package covers the four real Jira hierarchy mechanisms (`fields.subtasks` via parse, `fields.parent` via parse, modern `parent = "KEY"` JQL via hierarchy, legacy `"Epic Link" = "KEY"` JQL via hierarchy) so an Epic crawl actually surfaces its children. `internal/render` renamed the existing "### Children" subsection (which had been rendering `issue.Subtasks`) to "### Sub-tasks" and introduced a new "### Children" subsection backed by `issue.Children`, populated by `internal/crawl` after JQL search. Companion to `full-prd.md` v0.3. |
| v5 | 2026-06-05 | Added `internal/devstatus` package (Jira Dev Status enrichment). Updated §3 layout, §4 with the new package paragraph, §5.1 with the new dependency rule, and §6 testability seam row. The package surfaces pull-request URLs that the standard issue GET response does not carry, via the undocumented `/rest/dev-status/1.0/issue/detail` endpoint. The documented `customfield_10000` development-summary field is parsed as a per-issue gate so zero-PR issues incur no API call. `internal/parse` gained `Issue.NumericID` (sourced from the top-level `id` field of the GET response) and the new `PullRequest` and `Reviewer` value types — parse still never populates `PullRequests`, which is mutated by the crawl after enrichment. `internal/render` added a new top-level `## Development` section between `## Description` and `## Relationships`, with a `### Pull requests` subsection. `internal/crawl` gained a `PRDiscoverer` interface and a new `CrawlWithEnrichers` entry point that accepts both a `ChildDiscoverer` and a `PRDiscoverer`; the legacy `Crawl` and `CrawlWithDiscoverer` are preserved as thin wrappers for source compatibility. Companion to `full-prd.md` v0.4. |
| v6 | 2026-06-05 | Expanded `internal/devstatus` from "pull requests only" to all five Dev Status dataType values the Jira UI Development panel surfaces (`pullrequest`, `branch`, `commit`, `repository`, `build`) with a smart per-dataType gate that fixes a silent-miss bug discovered on PROJ-1578. The previous gate compared the `customfield_10000` summary's `pullrequest.overall.count` to zero and suppressed the call when zero; this missed any issue whose linked entities were only repositories/branches/commits/builds, and also missed any issue whose cached summary was stale (Atlassian's own response carried `isStale:true` in the reproducer). The new `SmartGate` returns the configured dataTypes whose summary count > 0; when the summary is missing, unparseable, OR reports zero for every dataType, it falls back to querying ALL configured dataTypes. `client.DevStatus` gained a `dataType` parameter (was hard-coded to `pullrequest`). `internal/parse` replaced `Issue.PullRequests []PullRequest` with `Issue.DevStatus DevStatusData` — a struct carrying typed `PullRequests`, `Branches`, `Commits`, `Repositories`, `Builds` lists; this is a breaking change to the typed Issue shape (no external users; renderer and crawler updated). `internal/render` extended the `## Development` section with four new subsections (`### Branches`, `### Commits`, `### Repositories`, `### Builds`) in canonical Jira-UI order, each independently elided when its list is empty. `internal/config` added `GOJIRA_DEV_STATUS_DATA_TYPES` (default `pullrequest,branch,commit,repository,build`) with a `oneof_dev_status_data_types` validator. `internal/crawl` renamed `PRDiscoverer` to `DevStatusEnricher` with a single `Enrich(ctx, issue) (parse.DevStatusData, error)` method. AC 24 from the prior commit was DELETED because it pinned the broken behavior; AC 26-30 added in its place. Companion to `full-prd.md` v0.5. |
| v7 | 2026-06-05 | Added §2.7 (signature honesty) and a new `docs/engineering-principles.md` file capturing the rule that every parameter influencing a function's runtime behavior must be declared in its signature. Constants embedded in a function body that *could* meaningfully differ between calls are not allowed; the signature would lie about the contract. Two concrete examples this rule has already caught: `client.DevStatus`'s hard-coded `dataType` value (fixed in v6), and a renderer helper that hard-coded a section name. The `.aider-desk/rules/10-go-engineering.md` agent rule file carries the same rule; the two files are kept in sync. No code changes in this commit. |
| v8 | 2026-06-05 | Fixed two bugs in the Dev Status enrichment flow surfaced when re-crawling PROJ-1417. (1) `client.DevStatusResponse.Errors` was declared as `[]string`, modelled against the empty-array shape seen on PROJ-1573; production tenants returned object-shaped error entries (`{"code": <int>, "message": <string>, "userId": <string>}`) for `dataType=commit` and `dataType=build`, crashing `json.Unmarshal` with "cannot unmarshal object into Go struct field DevStatusResponse.errors of type string". Retyped to `[]json.RawMessage` with a doc comment applying the §2.7 signature-honesty rule to response models — the field name promises "errors" but the entry shape is not stable; the only honest model is "any valid JSON element". (2) The crawl orchestrator emitted `KindIssueFailed` (mapped to ERROR) when one of N Dev Status per-call requests failed even though the issue itself was rendered successfully with whatever data did come back. Added a new `KindDevStatusPartialFailure` event mapped to WARN in `slog_sink.go`; updated `internal/crawl/crawl.go` to emit the new kind instead, and pinned the contract in the `DevStatusEnricher` doc comment that `Summary.Failed` is NOT incremented for these events. The combined effect is that the PROJ-1417 case now renders a `## Development` section with the entities that did come back, the partial-call failure surfaces as a Warn-level `devstatus.partial_failure` log line, and the crawl summary correctly reports `Fetched=1, Failed=0`. AC 26 (PROJ-1578 repositories surfaced) is preserved unchanged: the bug fix is orthogonal to the smart-gate behavior locked in by v6. Added: `TestDevStatus_ObjectErrorsDoNotCrashUnmarshal` in `client/client_test.go`, a Kind→Level case in `internal/events/slog_sink_test.go`, `TestCrawl_DevStatusPartialFailure` in `internal/crawl/crawl_test.go`, and `TestAC31_DevStatusPartialFailureIsWarning` in `acceptance_test.go`. PRD §7 (Dev Status enrichment) gained one sentence describing the new event kind. PRD §13 unchanged (the existing ACs pin rendered-output behavior, not event taxonomy). Companion to `full-prd.md` v0.5 (no version bump; the bug-fix is consistent with the v0.5 PRD text). |
| v10 | 2026-06-05 | Custom-fields rendering rewrite. `client.GetIssue` gained an `expand []string` parameter and `internal/fetch` passes `[]string{"names"}` explicitly so the Jira API response carries the top-level `"names"` map of field ID → human-readable label (e.g. `customfield_10115 → "Sprint"`); the signature-honesty rule (§2.7) is the worked example here — there is no baked-in default inside `GetIssue`. `internal/parse` gained `Issue.Names map[string]string` populated from that top-level object; the parser tolerantly skips non-string/null entries individually so one malformed entry cannot erase every label. `internal/render` rewrote the `## Custom fields` block: a new `classifyCustomField` helper sorts each value into one of four kinds — null / primitive / structured / invalid — and the renderer formats them accordingly: primitives render inline (`- **Sprint**: "value"`), structured (JSON object/array) values render inside a fenced ` ```json ` code block indented two spaces under the bullet (so the fence does not terminate the surrounding list), invalid (not-valid-JSON) values render in a plain ` ``` ` fence with no language tag — the latter case catches Atlassian's `customfield_10000` `{key=value, json={...}}` notation honestly. `RenderIssue` gained a positional `renderNullCustomFields bool` parameter threaded from `internal/config`'s new `RenderNullCustomFields` field (sourced from `GOJIRA_RENDER_NULL_CUSTOM_FIELDS`, default `false`); null-valued custom fields are skipped under the default, surfaced under `true`. `internal/config` added `GOJIRA_RENDER_NULL_CUSTOM_FIELDS`; `cmd/gojira` added the `--render-null-custom-fields` flag wired via the existing `mapValueSource` pattern. Per-package LOC delta: `client/client.go` +~30 / `internal/fetch/fetch.go` +~10 / `internal/parse/parse.go` +~60 / `internal/render/render.go` +~110 -25 / `internal/config/config.go` +~12 / `cmd/gojira/main.go` +~20. Tests added: `TestGetIssue_WithExpand` (client), `TestParseNames{Present,Absent,SkipsNonStringEntries}` (parse), `TestClassifyCustomField` plus six `TestRenderIssue_CustomFields*` (render), `TestAC32_CustomFieldsRenderedWithHumanLabels` (acceptance). The existing `internal/render/testdata/unknown_custom_field.md` golden was updated to the new fenced-block layout; AC 13 still passes unchanged because its assertions check for substring presence, not exact format. PRD §6 gained the `GOJIRA_RENDER_NULL_CUSTOM_FIELDS` row and §13 gained AC 32. Companion to `full-prd.md` v0.7. |
| v11 | 2026-06-05 | Extended `internal/render`'s `classifyCustomField` helper with a second pass for JSON-string values whose decoded contents are themselves structured. Fixes the PROJ-1578 dogfooding gap where Atlassian's `customfield_10000` Dev Status summary arrived JSON-string-encoded — the v10 classifier saw "primitive string" and rendered it as one giant stringified line. The function signature is UNCHANGED (still returns `(kind string, pretty string, indented bool)`); only the classification logic and the set of returned `kind` values grow. New `kindStringStructured = "string-structured"` constant added; the renderer's switch grows one case that emits the same plain ` ``` ` fence as the existing `kindInvalid` branch (the two kinds stay separate so future divergence — e.g. a debug note for string-structured values — is a one-case change). New helpers in the same file: `classifyJSONStringContents` (recursive inner-string classifier), `looksStructured` (the `{`, `[`, `=`, `\n` heuristic), `isJSONStructured` (whether validated JSON bytes start with `{` or `[`). The four inner-string outcomes pinned: (a) inner is valid JSON object/array → `kindStructured`, pretty-printed; (b) inner is valid JSON primitive → `kindPrimitive`, outer JSON-string quotes preserved via `strconv.Quote`; (c) inner is not valid JSON but `looksStructured` → `kindStringStructured`, outer quotes stripped, inner content verbatim in a plain ` ``` ` fence; (d) inner is not valid JSON and not structured → `kindPrimitive`, outer quotes preserved. Per-package LOC delta: `internal/render/render.go` +~115 -10 / `internal/render/render_test.go` +~120 / `acceptance_test.go` +~85 / `testdata/acceptance/ac33_dev_status_summary_field.json` +21 (new). Tests added: 9 new rows in `TestClassifyCustomField`, 1 new `TestRenderIssue_CustomFieldsStringStructuredAsPlainFence`, 1 new acceptance test `TestAC33_DevStatusSummaryFieldRendersAsCodeBlock`. PRD §13 added AC 33. No new config knob: the `looksStructured` heuristic is the policy, intentionally not user-toggleable per the spec's explicit non-goal. Companion to `full-prd.md` v0.8. |
| v12 | 2026-06-06 | Added an in-place pretty-printer for Atlassian's mixed Java `Map.toString()`+JSON notation used in the Dev Status summary blob (and structurally similar custom field values). Closes the last legibility gap discovered through PROJ-1573 dogfood: under v11 the `kindStringStructured` branch still emitted the entire blob as a single ` ```json `-fenced line. The new `prettifyAtlassianBlob(s string) (pretty string, ok bool)` helper in `internal/render/render.go` walks the bytes with a small state machine — tracking brace depth (`{`/`[` increment, `}`/`]` decrement), inserting a newline+two-space indent after every container open and before every container close, splitting on `, ` at the current depth, and collapsing empty `{}`/`[]` to a single token. When the literal substring `json=` is encountered followed by `{` or `[`, the helper identifies the balanced JSON range via a companion `findJSONValueEnd` scanner (which respects JSON string literals and `\\` escapes so braces inside quoted strings do not confuse the depth tracker) and delegates the inner payload to `encoding/json`'s `json.Indent` with the current outer indent as its prefix — yielding proper JSON pretty-printing inside the Java-notation wrapping. The walker is single-pass, allocates one `bytes.Buffer`, and returns `(input, false)` unchanged on any structural surprise (unbalanced braces, unexpected EOF, `json=` followed by content that fails `json.Indent`); the renderer's `kindStringStructured` branch checks `ok` and falls back to the verbatim single-line render so partial-mangled output is never returned. The ` ```json ` language tag is preserved because the bulk of the content is still JSON-shaped; adding whitespace does not change that. Scope: changes only the `kindStringStructured` rendering branch — `kindStructured` keeps using `json.Indent` as today, and `kindInvalid` keeps using a plain ` ``` ` fence. No new config knob: the multi-line layout is unconditional when the walker succeeds. Per-package LOC delta: `internal/render/render.go` +~190 -10 / `internal/render/export_test.go` +~10 / `internal/render/render_test.go` +~180 / `acceptance_test.go` +~15 -10 (assertion updates) / no fixture changes. Tests added: 1 new `TestPrettifyAtlassianBlob` with 13 table-driven subtests (empty input; balanced empty object/array; simple/nested Atlassian Java notation; embedded JSON object/array; the PROJ-1573 realistic blob; four malformed-input rows asserting the ok=false verbatim-fallback contract; JSON-string-containing-braces depth-tracker stress). The existing `TestRenderIssue_CustomFieldsStringStructuredAsJSONFence` and `TestAC33_DevStatusSummaryFieldRendersAsCodeBlock` were UPDATED to assert the new multi-line shape (`repository={\n`, `"isStale": true` with colon-space, `  ```json\n  {\n` for the bullet-indented fence opening); the older verbatim-content substrings (`{repository={count=1`, `"isStale":true`) were replaced because the walker breaks them across lines. PRD §13 AC 33 reworded to reflect the multi-line indented render in a ` ```json ` fence. Companion to `full-prd.md` v0.9. |
| v9 | 2026-06-05 | **Removed the `customfield_10000` smart gate entirely from `internal/devstatus`.** The gate had caused two silent-miss bugs in three commits. The original PROJ-1578 reproducer (v6) was supposed to fix the bug by extending the gate from "pullrequest only" to per-dataType counts; the v6 gate then trusted the summary's per-dataType counts even when Atlassian's own `"isStale": true` flag was set, and on the literal PROJ-1578 issue the summary said `repository.count=1` and zero for every other dataType while the Jira UI showed a PR, branches, commits, and builds — all of which the gate silently dropped. The user's preference (verbatim): *"Honestly, let's just go with option B. Frankly, I'd rather even drop it — I'd prefer it to make those requests and then log properly if they can't reach them or something."* The new contract: when `IncludeDevStatus=true` and `NumericID` is non-empty, every configured `(application, dataType)` pair is queried unconditionally. Cost: at most five extra HTTP requests per issue. Benefit: "no data reached me" can no longer be silently caused by a stale summary cache. Removed: `SmartGate`, `ParseSummaryCounts`, `ParseSummaryCount` (deprecated v0 shim with zero external callers), `SummaryCounts`, `devSummary`, `devSummarySection`, `extractInnerJSON`, `nonzeroDataTypes`, `normaliseConfigured`, `orderedConfigured` (replaced by a single internal `orderedDataTypes` helper that just intersects configured with `CanonicalDataTypes`). `Enricher.Enrich` is now ~80 lines (was ~150) and reads top-to-bottom. The crawl orchestrator's `KindDevStatusPartialFailure` emission is preserved (added in v8) and its message wording was split into two cases (partial data vs no entities discovered); `Summary.Failed` is still NOT incremented for partial-failure events. Acceptance criteria: AC 30 (`SummaryStaleFallback`) is DELETED — it pinned the stale-fallback gate behavior that no longer exists. AC 28 (PROJ-1578 repositories surfaced) keeps the same fixture and `index.md` assertion but now asserts that ALL FIVE dataTypes are queried (was: "only repository"); the repository surfaces because its dataType call went out unconditionally. AC 23/26/27/29 each set the four non-target dataTypes to the canonical empty response (the default body is PR-flavoured) and assert five calls per issue. AC 25 (`IncludeDevStatus=false`) is unchanged. Test file `internal/devstatus/devstatus_test.go` lost ~400 lines of gate/parser test surface; one new table-driven `TestEnricher_AlwaysCallsAllDataTypes` enumerates the four classes of summary state the legacy gate branched on and asserts all five dataTypes are queried in every case. PRD §7 (Crawl Semantics → Dev Status enrichment) rewritten to drop smart-gate language; PRD §13 deletes AC 30 and simplifies AC 23/26/27/28/29. Companion to `full-prd.md` v0.6. |
