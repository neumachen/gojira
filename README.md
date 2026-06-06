# gojira

A Go CLI and library for working with Jira Cloud from the command
line. The v0.1.0 release ships `gojira crawl`, which recursively
mirrors a Jira issue graph into Markdown — including the issue's
hierarchy, development metadata (branches, commits, pull requests,
builds, repositories), and human-labelled custom fields. Future
releases will add subcommands for targeted fetching, ticket
creation and update, and other workflows that today require the
Jira UI.

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

# 2. Configure credentials. The CLI reads env vars; either export
#    them or use a .env file.
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
| `--log-level`                 | `GOJIRA_LOG_LEVEL`            | `info`        | One of: `error`, `warn`, `info`, `debug`. |
| `--log-format`                | `GOJIRA_LOG_FORMAT`           | `text`        | One of: `text` (human-readable), `json` (one JSON object per line). |

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

The four named library capabilities (`Classify`, `LoadConfig`,
`FetchAndRender`, `Crawl`) plus the type aliases for `Config`,
`Summary`, `Sink`, and `Event` are documented in
[`gojira.go`](./gojira.go)'s package doc.

## Known limitations (v0.1.0)

- Jira Cloud only. Jira Server / Data Center is out of scope for
  the entire product.
- Comments are not yet rendered (the field is fetched but the
  current renderer ignores it; landing in v0.2).
- The Jira Dev Status API used for development metadata is
  semi-undocumented. It has been stable for ~10 years because
  Atlassian's UI uses it, but no SLA is offered. Disable with
  `--include-dev-status=false` to opt out.
- The CLI ships only one subcommand: `crawl`. See the roadmap.

## Roadmap

Future releases anticipated:

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
