# Jira releases and Fix Versions

Managing Jira project **Versions** — the entries on a Jira project's
"Releases" page — and attaching them to issues as Fix Versions. Versions
are project-scoped: they belong to a project, not to a single issue.

> This is **not** about gojira's own build version. For how the gojira
> binary reports its build identity and how a maintainer cuts a GitHub
> release, see [./releasing.md](./releasing.md).

## Workflow

gojira can cut a Jira release and attach it to an issue directly,
replacing hand-rolled `curl` calls against the Jira REST API. The
end-to-end flow is cut → attach → transition:

```bash
# 1. Cut the release on the Jira project (creates a project Version).
gojira release create --project EXAMPLE --name Release-abc1234 \
  --released --release-date 2000-01-01

# 2. Attach it to an issue as a Fix Version (by name or by id).
gojira update EXAMPLE-123 --fix-version Release-abc1234
# or: gojira update EXAMPLE-123 --fix-version-id 10001

# 3. Move the issue through its workflow with the EXISTING transition command.
gojira transition EXAMPLE-123 --to-status "Done"
```

## Command reference

For the full flag list see the README's
[`## Jira releases: gojira release`](../README.md#jira-releases-gojira-release)
section. In brief:

- `gojira release create` — cut a Version (requires `--name` and one of
  `--project` / `--project-id`).
- `gojira release update <VERSION-ID>` — update only the fields you pass.
- `gojira release list --project <KEY|id>` — list a project's versions.

Fix Versions on `gojira create` / `gojira update`:

- `--fix-version <name>` / `--fix-version-id <id>` — attach by name or id
  (repeatable; a set-replace on `update`).
- `--add-fix-version <id>` / `--remove-fix-version <id>` — on `update`
  only, add/remove incrementally by id (repeatable).

## No deletion

Version **deletion is intentionally unsupported**, consistent with
gojira's no-delete stance.

## Beyond the CLI

The same capability is available through the other surfaces: MCP tools
`list_versions` (read) and `create_version` / `update_version` (write,
gated behind `mcp.allow_writes`), and gRPC `CreateVersion` /
`UpdateVersion` / `ListVersions`.
