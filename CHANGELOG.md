# Changelog

All notable changes to gojira are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [v0.3.0] — 2026-06-12

### Added

- `gojira init --local` scaffolds a project-local `./gojira.yaml` in the
  current directory (a complete, self-sufficient config written `0600`),
  and adds it to `.gitignore` when one is present. `gojira init`
  (or `--global`) continues to write the global XDG config; the two
  flags are mutually exclusive and `--local` never touches the global
  config.

### Changed

- The configuration cascade now layers the global config and a
  project-local `./gojira.yaml` field-by-field when no `--config` or
  `$GOJIRA_CONFIG_FILE` is given: `defaults < global config.yaml <
  local ./gojira.yaml < env < flags`. A local file overrides the
  global per field and inherits any field it omits.
  **Behavior change:** previously a local `./gojira.yaml` fully shadowed
  the global config; now the global config is merged underneath it. An
  explicit `--config`/`$GOJIRA_CONFIG_FILE` still pins a single file
  with no layering.
- The "no configuration found" error now also points at
  `gojira init --local` and names both the global and project-local
  config locations.

## [v0.2.0] — 2026-06-12

### Added

- `gojira serve` — a long-lived gRPC server exposing the full
  capability surface (Classify, GetIssue, Crawl, GetGraph, and the
  write RPCs) over `gojira.v1.Gojira`. Single-tenant, loopback by
  default, no TLS/auth (trusted-network scope).
- `gojira mcp` — a Model Context Protocol server over stdio so AI
  hosts can use gojira as MCP tools. Config-driven `self`/`bridge`
  modes; mutating tools gated behind `mcp.allow_writes`.
- Write operations across the library, gRPC, CLI, and MCP surfaces:
  `create`, `update`, `comment`, `transitions`, and `transition`
  (with `--dry-run` previews for create/update). Issue deletion is
  intentionally unsupported.
- Graph export: `crawl --graph` writes `graph.json` and `graph.d2`
  (D2 source); the library exposes `CrawlGraph` and the gRPC service
  a `GetGraph` RPC returning the discovered graph in memory.
- `gojira init` — scaffolds a `0600` config file at the XDG path with
  a no-echo token prompt; a require-config guard now fronts every
  Jira-touching command.
- Crawl observability: a five-level log ladder (error/warn/info/
  debug/trace) with structured correlation attributes and an
  end-of-run `crawl.measurement` summary; credentials are always
  redacted, even at trace.
- CI now prints first-party statement coverage; the README carries
  CI/CD/coverage/Go-reference/report-card/license badges.

### Changed

- **BREAKING (layout):** Reorganized packages by importability. The
  reusable `classify`, `client`, and `log` packages moved under `pkg/`
  (import paths gain a `/pkg/` segment). The library facade
  (`github.com/neumachen/gojira`) is unchanged and remains at the root.
- **BREAKING (internal):** `internal/grpcserver` is now `internal/grpc`
  with an encapsulated `grpc.Serve(ctx, cfg)`; `internal/mcpserver` is
  now `internal/mcp` with `mcp.Serve(ctx, cfg)`. The gRPC and MCP SDKs
  are fully hidden behind these packages — the command layer no longer
  imports them.
- **BREAKING (internal):** All CLI command wiring moved from
  `cmd/gojira` (package main) into `internal/cli`. `cmd/gojira/main.go`
  is now a thin entrypoint calling `cli.Run`.
- **BREAKING (config):** The gRPC bind address is now a first-class
  `Config.ServerAddress` field resolved by the configuration cascade
  (default `127.0.0.1:50051`). The separate `gojira.ServerConfig` and
  `gojira.LoadServerConfig` accessors were removed; read
  `Config.ServerAddress` from a loaded `Config` instead.

All changes are pre-1.0 (alpha); no external consumers depend on the
previous paths.

## [v0.1.0] — 2026-06-06

The first tagged release. Ships the CLI's `crawl` subcommand
and the matching Go library surface.

### Added

- `gojira crawl` subcommand: recursive mirroring of a Jira issue
  and the graph reachable from it. Renders each issue as
  `<KEY>/index.md` with metadata, description, relationships, a
  `## Development` panel (pull requests, branches, commits,
  repositories, builds), and human-labelled custom fields. Per-
  issue `references/outbound.md` summarises outbound references.
- Hierarchy discovery: modern JQL `parent = "KEY"` plus legacy
  Epic Link custom-field search (auto-detected from
  `/rest/api/3/field`).
- Dev Status enrichment: queries the Jira Dev Status API for all
  five dataType values (pullrequest, branch, commit, repository,
  build), honouring user-configurable application types.
- Custom-field rendering: human-readable labels (via
  `expand=names`), pretty-printed JSON code blocks for
  structured values, the Atlassian Map.toString pretty-printer
  for the Dev Status summary blob, and a `--render-null-custom-
  fields` flag to control null-value visibility.
- Go library surface: `Classify`, `LoadConfig`, `FetchAndRender`,
  `Crawl`. Third-party programs can embed gojira directly via
  `github.com/neumachen/gojira`.
- Container image: multi-stage Dockerfile on Alpine 3.23, plus
  `docker-compose.yml` for one-shot invocation via
  `docker compose run --rm gojira crawl <KEY>`.

### Known limitations

- Jira Cloud only.
- Comments not yet rendered (fetched but ignored).
- Dev Status API is undocumented; disable with
  `--include-dev-status=false` to opt out.
- Only `crawl` is implemented; targeted fetch, ticket write
  operations, and JQL-driven crawls are anticipated for future
  releases.

[v0.1.0]: https://github.com/neumachen/gojira/releases/tag/v0.1.0
