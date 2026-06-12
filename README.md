# gojira

A Go CLI and library for working with Jira Cloud from the command
line. Its flagship `gojira crawl` recursively mirrors a Jira issue
graph into Markdown — including the issue's hierarchy, development
metadata (branches, commits, pull requests, builds, repositories),
and human-labelled custom fields — and can export the discovered
graph as JSON and D2. Beyond crawling, gojira also ships a gRPC
server (`gojira serve`), a Model Context Protocol server for AI
hosts (`gojira mcp`), and write operations to create and update
issues, add comments, and run workflow transitions. The same
engine is available as an embeddable Go library.

> Pre-1.0 status. APIs may change between minor versions until
> v1.0 is tagged.

## Install

### Go install

```sh
go install github.com/neumachen/gojira/cmd/gojira@v0.1.0
```

### Docker

```sh
# Pull (when published to a registry) or build locally:
docker build -t gojira:v0.1.0 .
```

A docker-compose.yml is provided for convenience; see "Docker
Compose" below.

## Quick start

```sh
# 1. Generate a Jira Cloud API token at:
#      https://id.atlassian.com/manage-profile/security/api-tokens

# 2. Configure credentials. The simplest path is `gojira init`, which
#    writes a 0600 config file at ~/.config/gojira/config.yaml — see
#    "First run: gojira init" below. The CLI also reads env vars
#    directly: either export them or use a .env file.
gojira init   # interactive; or pass --site/--user/--token as flags
# alternative: env-only configuration
cp .env.example .env
$EDITOR .env   # fill in GOJIRA_SITE, GOJIRA_USER, GOJIRA_TOKEN

# 3. Crawl a single Jira issue and its reachable graph.
gojira crawl PROJ-123

# 4. The Markdown output appears under ./out by default.
ls out/PROJ-123/
```

### Docker Compose

```sh
# After ./out is created by your first run, this works as a
# convenient wrapper:
docker compose run --rm gojira crawl PROJ-123
```

## What `gojira crawl` does

Starting from one Jira issue key, the crawler:

1. Fetches the issue via the Jira Cloud REST API v3
   (`GET /rest/api/3/issue/{key}?expand=names`).
2. Parses the issue's description from Atlassian Document Format
   into Markdown.
3. Renders an `index.md` for the issue under
   `<output-dir>/<KEY>/index.md` with metadata, description,
   relationships, the `## Development` panel (pull requests,
   branches, commits, repositories, builds), and human-labelled
   custom fields.
4. Recursively follows links: subtasks, parents, issue links,
   hierarchy children (via JQL `parent = "KEY"` plus Epic Link),
   and Jira-flavoured links inside the description. Each
   discovered key is fetched and rendered the same way.
5. Recognizes GitHub pull request URLs as PR references even when
   no Jira key is present in the PR title or branch.
6. Writes a per-issue `references/outbound.md` summarising every
   outbound reference the issue produced.
7. Honours configurable caps on depth, issue count, time, and
   API concurrency.

## CLI flags

The `crawl` subcommand accepts these flags. Each maps to an env
var of the same name in uppercase with a `GOJIRA_` prefix; the
flag overrides the env var when both are set.

| Flag                          | Env var                       | Default       | What it does |
|---|---|---|---|
| `--site`                      | `GOJIRA_SITE`                 | (required)    | Jira Cloud site URL, e.g. `https://your-site.atlassian.net`. |
| `--user`                      | `GOJIRA_USER`                 | (required)    | Atlassian account email. |
| `--token`                     | `GOJIRA_TOKEN`                | (required)    | Atlassian API token. |
| `--output-dir`                | `GOJIRA_OUTPUT_DIR`           | `./out`       | Output root directory. |
| `--depth-limit`               | `GOJIRA_DEPTH_LIMIT`          | `0` (no cap)  | Max crawl depth from the start issue. |
| `--issue-cap`                 | `GOJIRA_ISSUE_CAP`            | `500`         | Max issues to fetch per run. |
| `--time-cap`                  | `GOJIRA_TIME_CAP_SECONDS`     | `0` (no cap)  | Max wall-clock seconds per run. |
| `--concurrency`               | `GOJIRA_CONCURRENCY`          | `3`           | Concurrent Jira API requests. |
| `--refetch`                   | `GOJIRA_REFETCH`              | `false`       | Re-fetch issues that already exist on disk. |
| `--include-comments`          | `GOJIRA_INCLUDE_COMMENTS`     | `false`       | Fetch issue comments (v0.1.0 ignores them; reserved for v0.2). |
| `--include-children`          | `GOJIRA_INCLUDE_CHILDREN`     | `true`        | Discover hierarchy children via JQL parent search. |
| `--child-search-limit`        | `GOJIRA_CHILD_SEARCH_LIMIT`   | `100`         | Max children to discover per parent. |
| `--epic-link-field`           | `GOJIRA_EPIC_LINK_FIELD`      | (auto-detect) | Tenant's Epic Link custom-field ID. |
| `--include-dev-status`        | `GOJIRA_INCLUDE_DEV_STATUS`   | `true`        | Query the Jira Dev Status API for development metadata. |
| `--dev-status-applications`   | `GOJIRA_DEV_STATUS_APPLICATIONS` | `GitHub`   | Comma-separated Dev Status integration types. |
| `--dev-status-data-types`     | `GOJIRA_DEV_STATUS_DATA_TYPES` | `pullrequest,branch,commit,repository,build` | Comma-separated dataType values to query. |
| `--render-null-custom-fields` | `GOJIRA_RENDER_NULL_CUSTOM_FIELDS` | `false`  | Include custom fields whose value is JSON null. |
| `--graph`                     | `GOJIRA_EMIT_GRAPH`           | `false`       | Write `graph.json` and `graph.d2` (D2 source) at the output-dir root. |
| `--log-level`                 | `GOJIRA_LOG_LEVEL`            | `info`        | One of: `error`, `warn`, `info`, `debug`, `trace`. |
| `--log-format`                | `GOJIRA_LOG_FORMAT`           | `text`        | One of: `text` (human-readable), `json` (one JSON object per line). |
| `--config`                    | `GOJIRA_CONFIG_FILE`          | (discovered)  | Path to a YAML config file (see [Configuration](#configuration)). |

## First run: `gojira init`

Every Jira-touching command (`crawl`, `serve`, `create`, `update`,
`comment`, `transitions`, `transition`) requires some form of
configuration before it will contact Jira. The simplest way to provide
it is the `gojira init` subcommand, which scaffolds a config file at
the XDG global path (`$XDG_CONFIG_HOME/gojira/config.yaml` or, when
`XDG_CONFIG_HOME` is unset, `~/.config/gojira/config.yaml`) with
`0600` permissions because it contains your Jira API token.

```sh
gojira init \
  --site https://your.atlassian.net \
  --user you@example.com \
  --token <api-token>
```

Omitted required values are prompted for interactively. The token is
read **without echo** on a real terminal (via `golang.org/x/term`) and
falls back to a warned-echo read on non-terminals (CI pipes, etc.).
The `--output-dir` and `--server-address` flags accept the documented
defaults (`./out` and `127.0.0.1:50051`) when omitted.

`gojira init` refuses to overwrite an existing config file unless you
pass `--force`. The written YAML has the same shape as
[`gojira.example.yaml`](./gojira.example.yaml) and is consumed by the
same loader the cascade below documents.

### How a command finds its configuration

A guarded command will run iff **any one** of the following is true:

1. A config file is discovered through the cascade (see
   [Config-file discovery](#config-file-discovery) below): an explicit
   `--config` value, `$GOJIRA_CONFIG_FILE`, `./gojira.yaml`, or the XDG
   global config file.
2. The `--config <path>` flag was passed.
3. The `--site`, `--user`, and `--token` flags were ALL passed.
4. The required env vars are present: `GOJIRA_CONFIG_FILE`, or all
   three of `GOJIRA_SITE`, `GOJIRA_USER`, and `GOJIRA_TOKEN`.

If none of (1)-(4) hold, the command exits with a clear error pointing
at `gojira init`. The `init`, `help`, and `--version` invocations are
exempt from this guard so they remain usable from a no-config state.

## Configuration

gojira supports three configuration surfaces — embedded defaults, an
optional YAML config file, and `GOJIRA_*` environment variables — plus
the CLI flags documented above. They compose into one effective
configuration through a documented cascade:

```text
embedded defaults < YAML config file < GOJIRA_* env vars < CLI flags
```

A value at any layer overrides the same value at every lower layer; a
value absent at every layer keeps its embedded default.

### Config-file discovery

When the YAML file is not supplied explicitly, gojira searches the
following locations in order and uses the first existing regular file:

1. `--config <path>` (CLI flag)
2. `$GOJIRA_CONFIG_FILE`
3. `./gojira.yaml` (current working directory)
4. `$XDG_CONFIG_HOME/gojira/config.yaml`
5. `~/.config/gojira/config.yaml`

Candidates 1 and 2 are **explicit**: when set but the file does not
exist, gojira exits with a hard error so a misconfigured invocation
fails fast. Candidates 3-5 are **implicit**: a missing file there
simply falls through to the next candidate, and a fully absent chain
falls through to defaults plus environment variables (not an error).

A starter file lives at [`gojira.example.yaml`](./gojira.example.yaml).
Copy it to one of the locations above, edit the values you care about,
and delete the blocks you want to leave at their defaults. The file's
first line embeds a `yaml-language-server` directive so editors like
VS Code (with the YAML extension) and Neovim get autocomplete and live
validation against the embedded JSON Schema at
[`internal/config/config.schema.json`](./internal/config/config.schema.json).

Quick start with a config file:

```sh
# Copy the example and edit it.
cp gojira.example.yaml gojira.yaml
$EDITOR gojira.yaml

# Supply the secret out of band so it never lands in version control.
export GOJIRA_JIRA_API_TOKEN="$(security find-generic-password -s gojira -w)"

# Crawl. --config is optional; ./gojira.yaml is auto-discovered.
gojira crawl --config gojira.yaml PROJ-1
```

### Canonical environment variables

The table below lists the canonical `GOJIRA_*` keys gojira reads.
Every key here has a YAML equivalent under the corresponding section
of `gojira.yaml`; pick whichever surface fits the deployment best.

| Env var                                       | YAML path                            | Default        |
|---|---|---|
| `GOJIRA_SCHEMA`                               | `schema`                             | `gojira.config.v1` |
| `GOJIRA_CONFIG_FILE`                          | (resolver only)                      | (discovered) |
| `GOJIRA_JIRA_BASE_URL`                        | `jira.base_url`                      | (required) |
| `GOJIRA_JIRA_EMAIL`                           | `jira.email`                         | (required) |
| `GOJIRA_JIRA_API_TOKEN`                       | `jira.api_token`                     | (required) |
| `GOJIRA_OUTPUT_DIR`                           | `output.dir`                         | (required) |
| `GOJIRA_CRAWL_DEPTH_LIMIT`                    | `crawl.depth_limit`                  | `0` |
| `GOJIRA_CRAWL_ISSUE_CAP`                      | `crawl.issue_cap`                    | `500` |
| `GOJIRA_CRAWL_TIME_CAP_SECONDS`               | `crawl.time_cap_seconds`             | `0` |
| `GOJIRA_CRAWL_CONCURRENCY`                    | `crawl.concurrency`                  | `3` |
| `GOJIRA_CRAWL_REFETCH`                        | `crawl.refetch`                      | `false` |
| `GOJIRA_CRAWL_INCLUDE_COMMENTS`               | `crawl.include_comments`             | `false` |
| `GOJIRA_CRAWL_INCLUDE_CHILDREN`               | `crawl.include_children`             | `true` |
| `GOJIRA_CRAWL_CHILD_SEARCH_LIMIT`             | `crawl.child_search_limit`           | `100` |
| `GOJIRA_CRAWL_EPIC_LINK_FIELD`                | `crawl.epic_link_field`              | (auto-detect) |
| `GOJIRA_CRAWL_INCLUDE_DEV_STATUS`             | `crawl.include_dev_status`           | `true` |
| `GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS`        | `crawl.dev_status_applications`      | `GitHub` |
| `GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES`          | `crawl.dev_status_data_types`        | `pullrequest,branch,commit,repository,build` |
| `GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS`      | `crawl.render_null_custom_fields`    | `false` |
| `GOJIRA_CRAWL_EMIT_GRAPH`                     | `crawl.emit_graph`                   | `false` |
| `GOJIRA_LOG_LEVEL`                            | `log.level`                          | `info` |
| `GOJIRA_LOG_FORMAT`                           | `log.format`                         | `text` |

### Deprecated aliases (still honored)

The v0.1 flat `GOJIRA_*` keys documented under [CLI flags](#cli-flags)
continue to work; gojira resolves them to their canonical Phase 0
equivalents during load. When both the canonical key and its alias
are set, the canonical key wins — set both only during a migration.

| Deprecated alias                  | Canonical replacement                       |
|---|---|
| `GOJIRA_SITE`                     | `GOJIRA_JIRA_BASE_URL`                      |
| `GOJIRA_USER`                     | `GOJIRA_JIRA_EMAIL`                         |
| `GOJIRA_TOKEN`                    | `GOJIRA_JIRA_API_TOKEN`                     |
| `GOJIRA_DEPTH_LIMIT`              | `GOJIRA_CRAWL_DEPTH_LIMIT`                  |
| `GOJIRA_ISSUE_CAP`                | `GOJIRA_CRAWL_ISSUE_CAP`                    |
| `GOJIRA_TIME_CAP_SECONDS`         | `GOJIRA_CRAWL_TIME_CAP_SECONDS`             |
| `GOJIRA_CONCURRENCY`              | `GOJIRA_CRAWL_CONCURRENCY`                  |
| `GOJIRA_REFETCH`                  | `GOJIRA_CRAWL_REFETCH`                      |
| `GOJIRA_INCLUDE_COMMENTS`         | `GOJIRA_CRAWL_INCLUDE_COMMENTS`             |
| `GOJIRA_INCLUDE_CHILDREN`         | `GOJIRA_CRAWL_INCLUDE_CHILDREN`             |
| `GOJIRA_CHILD_SEARCH_LIMIT`       | `GOJIRA_CRAWL_CHILD_SEARCH_LIMIT`           |
| `GOJIRA_EPIC_LINK_FIELD`          | `GOJIRA_CRAWL_EPIC_LINK_FIELD`              |
| `GOJIRA_INCLUDE_DEV_STATUS`       | `GOJIRA_CRAWL_INCLUDE_DEV_STATUS`           |
| `GOJIRA_DEV_STATUS_APPLICATIONS`  | `GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS`      |
| `GOJIRA_DEV_STATUS_DATA_TYPES`    | `GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES`        |
| `GOJIRA_RENDER_NULL_CUSTOM_FIELDS`| `GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS`    |
| `GOJIRA_EMIT_GRAPH`               | `GOJIRA_CRAWL_EMIT_GRAPH`                   |

`GOJIRA_OUTPUT_DIR`, `GOJIRA_LOG_LEVEL`, and `GOJIRA_LOG_FORMAT`
already use canonical names in v0.1 and need no migration.

## Output layout

```
out/
└── PROJ-123/
    ├── index.md
    └── references/
        └── outbound.md
```

Each fetched issue lives at `<KEY>/index.md`. Outbound references
discovered in that issue are summarised at
`<KEY>/references/outbound.md`. The `references/` directory keeps
the per-issue reference index out of the issue's own rendered
Markdown so a reader who wants the full graph view can find it,
but a reader who just wants the issue content sees only
`index.md`.

## Library usage

The same engine is available as a Go library. Third-party programs
can embed gojira in their pipelines without invoking the CLI
binary:

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/neumachen/gojira"
)

func main() {
    cfg, err := gojira.LoadConfig(map[string]string{
        "GOJIRA_SITE":       os.Getenv("GOJIRA_SITE"),
        "GOJIRA_USER":       os.Getenv("GOJIRA_USER"),
        "GOJIRA_TOKEN":      os.Getenv("GOJIRA_TOKEN"),
        "GOJIRA_OUTPUT_DIR": "./out",
    })
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }

    summary, err := gojira.Crawl(
        context.Background(), cfg, []string{"PROJ-123"}, nil,
    )
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    fmt.Printf("fetched %d issues; %d PRs discovered\n",
        summary.Fetched, summary.PRsFound)
}
```

The library facade groups its capabilities into classification
(`Classify`), configuration (`LoadConfig` and friends), fetch/render
(`GetIssue`, `FetchAndRender`), crawl (`Crawl`, `CrawlGraph`), and
write operations (`CreateIssue`, `UpdateIssue`, `AddComment`,
`ListTransitions`, `TransitionIssue`), plus type aliases for `Config`,
`Summary`, `Sink`, `Event`, and the graph model
(`GraphModel`/`GraphNode`/`GraphEdge`). All are documented in
[`gojira.go`](./gojira.go)'s package doc.

## Observability and tracing

`gojira crawl` ships a verbose, structured, correlatable observability
instrument designed for answering "where did the wall-clock go?" on a large
crawl. It's measurement-first: enabling it does not change traversal,
fetch logic, or on-disk output — only what is observed.

### Levels

`gojira` extends slog's standard four levels (`error`, `warn`, `info`, `debug`)
with a fifth, `trace`. The five levels carry intent-based meaning:

| Level | Meaning |
| ----- | ------- |
| `error` | Failures. |
| `warn` | Degraded enrichment, partial failures. |
| `info` | Operationally significant facts and all measurement data — phase/issue spans, per-HTTP-request summaries, the end-of-run `crawl.measurement` totals. A normal `--log-level info` run already shows time attribution. |
| `debug` | Durable diagnostics worth keeping even after a problem is solved — resolved state and decisions (skip-if-exists hits, epic-link field resolution). |
| `trace` | Traceability woven into the code — span lifecycles, the "because X therefore Y" fan-out reasoning, raw response bodies and full `net/http/httptrace` timings. |

Select with `--log-level trace` or `GOJIRA_LOG_LEVEL=trace`. Use `--log-format json`
to get machine-filterable JSON lines (one record per line).

### Correlation attributes

Every traced log line carries structured attributes so a single grep/jq
filter can reconstruct any subset of the run's work:

| Attribute | Meaning |
| --------- | ------- |
| `run_id` | Opaque short UID for this crawl invocation; on every line. |
| `ticket_id` | Jira issue key (e.g. `PROJ-1417`) — named to mirror Jira. Present on every line whose work concerns a specific issue. |
| `span_id` / `parent_span_id` | Opaque short IDs per unit of work, linking each unit to whoever enqueued it. Opaque, not hierarchical, because crawls can bleed across projects or boards. |
| `phase` | One of `fetch`, `parse`, `hierarchy_jql`, `dev_status`, `render`, `store`, `enqueue`. |
| `trace_stream` | `response` (HTTP/data side, from the round-tripper) or `stream` (orchestration side, from the crawl). |
| `depth`, `discovered_from`, `relation` | Fan-out lineage — present on `crawl.fanout` TRACE lines explaining why a key entered the queue. |

### Measurement summary

At end of run, `gojira crawl` emits a single INFO `crawl.measurement` line
with the per-phase wall-clock attribution:

```json
{"msg":"crawl.measurement","total_api_time_ms":31872,"total_duration_ms":32114,"call_counts":{"fetch":48,"parse":48,"hierarchy_jql":12,"dev_status":48,"render":48,"store":48},"time_by_phase_ms":{"fetch":18204,"hierarchy_jql":7411,"dev_status":4012,"parse":612,"render":1031,"store":602}}
```

The same totals are also folded into the [`crawl.Summary`](./gojira.go)
returned to library callers as `APICallCounts`, `APITimeByPhase`, and
`TotalAPITime`.

### Filtering examples

```bash
# All response-stream traces for one issue:
gojira crawl PROJ-1417 --log-level trace --log-format json 2>&1 \
  | jq 'select(.trace_stream=="response" and .ticket_id=="PROJ-1417")'

# Only the per-phase measurement summary:
gojira crawl PROJ-1417 --log-level info --log-format json 2>&1 \
  | jq 'select(.msg=="crawl.measurement")'

# Reconstruct the fan-out tree (TRACE):
gojira crawl PROJ-1417 --log-level trace --log-format json 2>&1 \
  | jq 'select(.msg=="crawl.fanout") | "\(.discovered_from) -[\(.relation)]-> \(.ticket_id)"'
```

### Credential redaction

`Authorization`, `Cookie`, `Proxy-Authorization`, `Set-Cookie`, and
`X-Atlassian-Token` headers are ALWAYS redacted in trace output, even at
`--log-level trace`. The raw token is never written to logs by design;
the redaction is audited by a unit test
(`TestRoundTripper_RedactsAuthorizationEvenAtTrace`).

## gRPC service (`gojira serve`)

In addition to the one-shot `crawl` subcommand, gojira can run as a
long-lived gRPC server that exposes its full capability surface —
classification, issue fetch, recursive crawl, in-memory graph export,
and write operations (create/update/comment/transitions) — to other
front-ends such as a TUI or the `gojira mcp` bridge.

```bash
# Start the server (reads the same GOJIRA_* config as `crawl`).
export GOJIRA_SITE="https://your-site.atlassian.net"
export GOJIRA_USER="you@example.com"
export GOJIRA_TOKEN="<api-token>"
gojira serve --address 127.0.0.1:50051
```

The server is **single-tenant**: one Jira identity is loaded at startup
from the same configuration cascade as `crawl`. It accepts concurrent
clients (each RPC is isolated) and is intended for a loopback or
otherwise trusted network — Phase 1 ships **no TLS and no
authentication**.

### Service `gojira.v1.Gojira`

| RPC | Type | Description |
| --- | ---- | ----------- |
| `Classify` | unary | Classify a bare key or URL into `JiraKey`, `JiraURL`, `GitHubPR`, or `External`. |
| `GetIssue` | unary | Fetch one issue. The response is a structured proto `Issue`, rendered Markdown, or JSON, selected by the request's `OutputFormat` (`STRUCTURED`, `MARKDOWN`, `JSON`). |
| `Crawl` | server-streaming | Recursively crawl from one or more start keys, streaming a `CrawlEvent` for each state transition. Issue content is written server-side to the configured output directory (streaming content over the wire is deferred to Phase 2). |
| `GetGraph` | unary | Crawl in-memory from one or more start keys and return the discovered issue graph as nodes and edges, without writing files. Mirrors the library's `CrawlGraph` and the CLI's `crawl --graph`. |
| `CreateIssue` | unary | Create an issue (project + type required; fields via summary/description/labels/parent and a `raw_fields` map). `dry_run` returns the request body the server would send, without creating anything. |
| `UpdateIssue` | unary | Edit fields on an existing issue. Honors `dry_run` like `CreateIssue`. |
| `AddComment` | unary | Append a comment (plain text, converted to ADF server-side) to an issue. |
| `ListTransitions` | unary | List the workflow transitions currently available for an issue (id, name, target status). |
| `TransitionIssue` | unary | Move an issue through a transition, selected by `transition_id` or by `target_status_name` (resolved server-side via `ListTransitions`). |

The proto contract is defined in
[`proto/gojira/v1/gojira.proto`](./proto/gojira/v1/gojira.proto) and the
generated Go bindings live under `gen/gojira/v1/`.

### Write operations

The `CreateIssue`, `UpdateIssue`, `AddComment`, and `TransitionIssue` RPCs let
clients mutate Jira through the same single-tenant identity the server loaded
at startup. Two design points are worth calling out:

- **Dry-run.** `CreateIssue` and `UpdateIssue` accept a `dry_run` flag. When
  set, the server builds and returns the exact JSON request body it *would*
  send to Jira (in `dry_run_body`) without performing the write — useful for
  previewing a mutation before committing to it.
- **Extensible fields.** Beyond the typed fields (summary, description,
  labels, parent), any Jira field — including tenant-specific custom fields —
  can be set through the `raw_fields` map (field id → raw JSON value). In the
  Go library this is the `WithField` / `WithRawFields` option; new fields never
  require a new method or signature change.

Errors carry Jira's detail: a 400 validation failure maps to gRPC
`InvalidArgument` with the failing field names in the message; a 409 (e.g. an
invalid workflow transition) maps to `FailedPrecondition`.

Issue **deletion is intentionally unsupported** — destructive removal is out of
scope for this phase.

### Server configuration

| Flag | Env var | Default | Description |
| ---- | ------- | ------- | ----------- |
| `--address` | `GOJIRA_SERVER_ADDRESS` | `127.0.0.1:50051` | gRPC server bind address. |
| `--config` | `GOJIRA_CONFIG_FILE` | (discovered) | Path to a YAML config file. |
| `--site` | `GOJIRA_SITE` | (required) | Jira Cloud site URL. |
| `--user` | `GOJIRA_USER` | (required) | Atlassian account email. |
| `--token` | `GOJIRA_TOKEN` | (required) | Atlassian API token. |
| `--output-dir` | `GOJIRA_OUTPUT_DIR` | `./out` | Output root directory for `Crawl`. |
| `--log-level` | `GOJIRA_LOG_LEVEL` | `info` | One of `error`, `warn`, `info`, `debug`, `trace`. |
| `--log-format` | `GOJIRA_LOG_FORMAT` | `text` | One of `text`, `json`. |

The server address can also be set in the YAML config file:

```yaml
server:
  address: 127.0.0.1:50051
```

The server stops gracefully on `SIGINT`/`SIGTERM`.

### Reference client

A minimal reference client ships at `cmd/gojira-client` for smoke-testing
a running server. It is a reference tool, not a production front-end.

```bash
go run ./cmd/gojira-client -address 127.0.0.1:50051 -classify PROJ-1147
go run ./cmd/gojira-client -address 127.0.0.1:50051 -key PROJ-1147 -format markdown
go run ./cmd/gojira-client -address 127.0.0.1:50051 -crawl PROJ-1147
go run ./cmd/gojira-client -address 127.0.0.1:50051 -create-project PROJ -create-type Task -create-summary "New task" -dry-run
go run ./cmd/gojira-client -address 127.0.0.1:50051 -comment PROJ-1147 -comment-text "Looks good"
go run ./cmd/gojira-client -address 127.0.0.1:50051 -transitions PROJ-1147
go run ./cmd/gojira-client -address 127.0.0.1:50051 -transition PROJ-1147 -to-status "In Progress"
```

### Regenerating the proto bindings

The generated code under `gen/` is committed. To regenerate after editing
the proto contract, run [buf](https://buf.build):

```bash
./scripts/gen-proto.sh   # runs `buf lint` then `buf generate`
```

## MCP server (`gojira mcp`)

`gojira mcp` runs gojira as a [Model Context Protocol](https://modelcontextprotocol.io/)
server over the **stdio transport** so AI hosts (Claude Desktop, Cursor,
Zed, …) can use gojira's tools directly. The host launches `gojira mcp`
as a subprocess and speaks JSON-RPC over its stdin/stdout. **stdout is
reserved for the protocol stream; all logs go to stderr.**

```sh
# Run from your AI host's mcpServers config (see below); for a manual
# smoke, you can drive a one-shot handshake yourself:
gojira mcp --config ~/.config/gojira/config.yaml < your-jsonrpc-frames
```

`gojira mcp` is guarded by the same require-config check as crawl/serve
(see [First run](#first-run-gojira-init)) and additionally requires
`mcp.mode` to be set — it exits 1 with a clear message when `mcp.mode`
is unset or not one of `self|bridge`.

### Modes

The mode is set via `mcp.mode` in the config file or `GOJIRA_MCP_MODE`
in the environment. It is **required** for `gojira mcp` and only for
`gojira mcp` (crawl/serve continue to load configs without an `mcp:`
section).

- **`self`** — gojira does the Jira work in-process via the library
  facade. The simplest mode; no extra server to run.
- **`bridge`** — gojira forwards each tool call to a running
  `gojira serve` gRPC server at `server.address` (default
  `127.0.0.1:50051`, override with `GOJIRA_SERVER_ADDRESS` or
  `--address`). This is the "one shared gRPC backend, many ephemeral
  MCP processes" topology: a single long-running `gojira serve` is
  the upstream, and every AI host spawns its own short-lived
  `gojira mcp` subprocess that bridges to it.

### Tools

The following MCP tools are always available (read-only):

- `classify` — classify an input as Jira key, Jira URL, GitHub PR URL, or external URL
- `get_issue` — fetch a single Jira issue with its outbound references
- `crawl` — recursively crawl Jira issues from one or more start keys; returns a summary on completion (emits MCP progress notifications per fetched issue when the host supplies a progress token)
- `get_graph` — crawl in-memory and return the discovered issue graph as `{nodes, edges}` (no files written)
- `list_transitions` — list workflow transitions available for an issue

The mutating tools are gated behind `mcp.allow_writes: true`
(default `false`). When `allow_writes` is false, the write tools are
**absent from `tools/list`** — an AI cannot mutate Jira until the
operator explicitly opts in:

- `create_issue`
- `update_issue`
- `add_comment`
- `transition_issue`

### Host configuration

Add an entry to your AI host's MCP servers configuration. The exact
file location varies by host (Claude Desktop:
`~/Library/Application Support/Claude/claude_desktop_config.json` on
macOS; Cursor and Zed have their own per-app paths) but the schema is
the same:

```json
{
  "mcpServers": {
    "gojira": {
      "command": "gojira",
      "args": ["mcp"],
      "env": {
        "GOJIRA_SITE": "https://your.atlassian.net",
        "GOJIRA_USER": "you@example.com",
        "GOJIRA_TOKEN": "your-api-token",
        "GOJIRA_MCP_MODE": "self"
      }
    }
  }
}
```

For **bridge** mode, set `"GOJIRA_MCP_MODE": "bridge"` and ensure a
`gojira serve` instance is running on the host; add
`"GOJIRA_SERVER_ADDRESS": "127.0.0.1:50051"` (or your chosen address)
if you do not use the default. The `env` block in the host config is
not the only configuration channel — `gojira init` (or a hand-written
`~/.config/gojira/config.yaml`) is equivalent, and the same
require-config guard described in [First run](#first-run-gojira-init)
applies: at least one of a discovered config file, `--config`, the
`--site/--user/--token` trio, or the equivalent `GOJIRA_*` env vars
must be present.

To expose the mutating tools to the host, add
`"GOJIRA_MCP_ALLOW_WRITES": "true"` to the `env` block (or set
`mcp.allow_writes: true` in your config file). Default is off.

## Known limitations (v0.1.0)

- Jira Cloud only. Jira Server / Data Center is out of scope for
  the entire product.
- Comments are not yet rendered (the field is fetched but the
  current renderer ignores it; landing in v0.2).
- The Jira Dev Status API used for development metadata is
  semi-undocumented. It has been stable for ~10 years because
  Atlassian's UI uses it, but no SLA is offered. Disable with
  `--include-dev-status=false` to opt out.
- The gRPC service (`gojira serve`) is single-tenant and ships without TLS or authentication; run it only on a loopback or trusted network. Streaming issue content over the wire, multi-tenancy, per-request config overrides, and TLS/auth are deferred to Phase 2.
- gRPC write operations (`CreateIssue`/`UpdateIssue`/`AddComment`/
  `TransitionIssue`) use the server's single startup identity; per-request
  credentials are deferred to a later phase. Issue deletion is not supported.
- Observability and tracing (`--log-level trace`) is opt-in; default `info`
  already shows the end-of-run measurement summary. Cross-process tracing
  (e.g. OpenTelemetry export across the gRPC boundary) is out of scope for
  this release.

## Roadmap

Future releases anticipated:

- A terminal UI (TUI) front-end over the gRPC API, as a gRPC client.
  (The MCP server already shipped — see [MCP server](#mcp-server-gojira-mcp).)
- Phase 2 service work: streaming rendered issue content over the
  wire, multi-tenant identities, per-request configuration
  overrides, and TLS/authentication.
- `gojira fetch` — targeted single-issue (or small-list) retrieval
  without recursive expansion. The same renderer; just no JQL
  parent search and no descent into linked issues.
- Write operations: create issues, update fields, post comments.
- JQL search: list issues matching a query and crawl the result
  set.
- Improved customisation: per-field rendering options, per-tenant
  field-name overrides.

No timelines committed. Direction only.

## Project documentation

- [docs/jira-markdown-crawler-design.md](./docs/jira-markdown-crawler-design.md)
  — the package-boundary design mini-doc.
- [docs/engineering-principles.md](./docs/engineering-principles.md)
  — the rule book contributors and AI agents follow.

## License

[MIT](./LICENSE)
