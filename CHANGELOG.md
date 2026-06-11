# Changelog

All notable changes to gojira are documented here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
