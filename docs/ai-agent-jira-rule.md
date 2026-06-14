# AI Agent Jira Rule

When an AI coding agent (AiderDesk, Claude, Codex, Cursor, Windsurf,
or any other agent) needs to interact with Jira from this repository
— or from any repository where `gojira` is installed and configured
— it MUST drive that interaction through `gojira`. It MUST NOT
shell out to `curl` against the Jira REST API, ask the user to paste
an API token into a prompt, store tokens in chat context, or
fabricate transition ids, status names, field names, or response
shapes.

This document is both a usage guide and a hard rule. The MUST /
MUST NOT / SHOULD / MAY keywords are used in the RFC 2119 sense.

## 1. When this rule applies

This rule is in force whenever ALL of the following hold:

- The agent is operating inside a checkout of this repository, or
  inside a repository where `gojira` is installed on `$PATH` or
  reachable via an MCP host.
- The agent's task involves reading, creating, updating, commenting
  on, or transitioning a Jira issue.
- A valid `gojira` configuration is available (see §4) — or the
  agent is in a position to ask the user to run `gojira init`.

If `gojira` is NOT available, the agent MUST stop and surface that
fact rather than fall back to raw REST calls.

## 2. The three surfaces

`gojira` exposes the same Jira capabilities through three surfaces.
Agents SHOULD prefer them in this order:

1. **MCP** — when the agent host speaks MCP (AiderDesk, Claude
   Desktop, etc.), use the `gojira mcp` server's tools. This is the
   highest-leverage surface because the host wires tool descriptions
   and JSON schemas directly into the model.
2. **CLI** — `gojira <subcommand>`. The right choice when the agent
   can run shell commands but the host has no MCP wiring.
3. **gRPC** — `gojira serve`. Use when integrating from another
   process or language, or when running gojira as a long-lived
   sidecar.

For in-process Go code, the library facade
(`github.com/neumachen/gojira`) is the right entry point — see §9.

The agent MUST NOT mix surfaces gratuitously inside a single task
(e.g. don't shell out to `curl` "just for this one call"). Pick a
surface and stick to it.

## 3. Setup (one-time)

### 3.1 Config cascade

`gojira` resolves configuration through a single ordered cascade:

```text
embedded defaults
  < XDG global config.yaml
  < local ./gojira.yaml
  < GOJIRA_ environment variables
  < CLI flags
```

In pure discovery mode, `gojira` reads
`$XDG_CONFIG_HOME/gojira/config.yaml` (typically
`~/.config/gojira/config.yaml`) and `./gojira.yaml` and merges them
field-by-field, local-over-global. Passing `--config <path>` or
setting `$GOJIRA_CONFIG_FILE` collapses this to single-file mode.

### 3.2 Required Jira fields

| YAML key             | Environment variable      | Meaning                            |
| -------------------- | ------------------------- | ---------------------------------- |
| `jira.base_url`      | `GOJIRA_JIRA_BASE_URL`    | `https://<tenant>.atlassian.net`   |
| `jira.email`         | `GOJIRA_JIRA_EMAIL`       | Atlassian account email            |
| `jira.api_token`     | `GOJIRA_JIRA_API_TOKEN`   | Atlassian API token                |

Every gojira env var carries the `GOJIRA_` prefix.

### 3.3 Safe ways to supply the API token

Agents MUST use one of these, in order of preference:

1. A `0600` `gojira.yaml` (local or global) that is gitignored.
2. The `GOJIRA_JIRA_API_TOKEN` environment variable, set in a shell
   the agent does NOT log or echo.
3. Only as a last resort: a write subcommand's `--token` flag.
   Passing `--token` on the command line risks leaking the token
   into shell history and process listings, so agents MUST NOT use
   it interactively when an env var or config file is available.

Agents MUST NOT paste tokens into chat, prompts, planning notes, or
memory tools.

### 3.4 `gojira init`

To scaffold configuration:

```bash
# Global, written to $XDG_CONFIG_HOME/gojira/config.yaml at 0600.
gojira init

# Project-local, written to ./gojira.yaml at 0600. If a .gitignore
# exists in cwd and does not list gojira.yaml, the entry is appended
# automatically; if no .gitignore exists, a warning is printed.
gojira init --local
```

## 4. Reading Jira

For multi-issue / graph reads, use `crawl`:

```bash
gojira crawl PROJ-123
```

This mirrors the issue graph into Markdown under the configured
output directory. Each crawled issue lives at `<ISSUE-KEY>/index.md`
plus sibling reference files. Agents MUST NOT hand-edit
`<ISSUE-KEY>/index.md` and expect a subsequent crawl to preserve the
edits — the file is regenerated.

For a single-issue read from an MCP host, prefer the `get_issue`
tool over `crawl` to avoid touching the filesystem.

## 5. Writing Jira (mutating actions)

Treat every write as a production mutation. When in doubt, run
`--dry-run` first to inspect the JSON body that would be sent.

### 5.1 Create

```bash
gojira create \
  --project PROJ \
  --type Task \
  --summary "Investigate flaky test" \
  --description "See attached crawl output." \
  --label triage --label observability \
  --dry-run
```

Required: `--project`, `--summary`. `--type` defaults to `Task`.
`--dry-run` prints the JSON body and exits without an HTTP call.

### 5.2 Update

```bash
gojira update PROJ-123 \
  --summary "Investigate flaky test (root cause known)" \
  --label triage --label resolved
```

`--label` REPLACES the issue's label set; it does not append. Only
flags the user actually sets are sent — unset flags do not clobber
existing values with empty strings. `--dry-run` is supported.

### 5.3 Comment

```bash
gojira comment PROJ-123 --text "Repro steps moved to the wiki."
```

`--text` is plain text and is converted to ADF server-side.

## 6. Workflow transitions — the two-step contract

This is the load-bearing section. The Jira API is id-driven and
transition ids are tenant- and workflow-specific. The agent MUST
follow this contract:

1. **List** the transitions currently available for the issue:

   ```bash
   gojira transitions PROJ-123
   ```

   Jira only returns transitions whose preconditions are met for
   the issue's CURRENT status, so the answer is specific to the
   issue's current workflow position.

2. **Pick** an id (or a status name) from that list.

3. **Execute** the transition:

   ```bash
   gojira transition PROJ-123 --id 21 --comment "shipping it"
   # or, equivalently, resolve the id server-side from a name:
   gojira transition PROJ-123 --to-status Done --comment "shipping it"
   ```

`--id` and `--to-status` are mutually exclusive; exactly one MUST
be supplied. `--comment` is optional and posts the comment as part
of the transition.

### 6.1 MUST NOTs for transitions

- MUST NOT hard-code a transition id observed in another project,
  workflow, or repository — ids do not transfer.
- MUST NOT guess a status name without first running
  `gojira transitions`.
- MUST NOT call `gojira transition` blind, skipping step 1.

### 6.2 Failure modes

- `204 No Content` — success.
- `400 Bad Request` — Jira's `errorMessages` are surfaced verbatim
  (e.g. `"Transition is not valid for current status."`). Treat
  this as a signal to re-run `gojira transitions` rather than
  retrying the same id.
- `409 Conflict` — concurrent modification. Re-list and retry.

## 7. MCP host wiring

`gojira mcp` runs an MCP stdio server. Two modes:

- `mcp.mode: self` — in-process. The MCP server uses the gojira
  library facade directly.
- `mcp.mode: bridge` — forwards to a running `gojira serve` over
  gRPC. Requires a configured server address.

Mutating tools are gated behind `mcp.allow_writes: true`. When
`allow_writes` is unset or false, the write tools are ABSENT from
the server's `tools/list` response — not merely refused at call
time. This is the right default for read-only agent sessions.

### 7.1 Tool surface

Always-on read tools:

- `classify`
- `get_issue`
- `crawl`
- `get_graph`
- `list_transitions`

Write tools (visible only when `mcp.allow_writes: true`):

- `create_issue`
- `update_issue`
- `add_comment`
- `transition_issue`

Issue deletion is intentionally unsupported. The way to close or
resolve an issue is to transition it.

### 7.2 Stdout-purity invariant

In `gojira mcp`, stdout carries JSON-RPC and nothing else. All log
records go to stderr. Agents and host integrations MUST NOT mix
log output into stdout; doing so corrupts the MCP transport.

## 8. gRPC

`gojira serve` exposes both read and write RPCs. The bind address
comes from `Config.ServerAddress` (default `127.0.0.1:50051`),
which is sourced from `App.Server.Address` via `App.ToConfig()`.
The generated proto lives under `gen/gojira/v1/`.

Write RPCs:

- `CreateIssue`
- `UpdateIssue`
- `AddComment`
- `ListTransitions`
- `TransitionIssue`

Use the gRPC surface from non-Go callers or when running gojira as
a sidecar. From Go code, prefer the library facade instead (§9).

## 9. Library facade (Go)

Import path: `github.com/neumachen/gojira`. Every call takes
`(ctx context.Context, cfg gojira.Config, ...)`:

- Reads: `gojira.Crawl`, `gojira.GetIssue`.
- Writes: `gojira.CreateIssue`, `gojira.UpdateIssue`,
  `gojira.AddComment`, `gojira.ListTransitions`,
  `gojira.TransitionIssue`.

Configuration sentinels `gojira.ErrConfigMissingRequired` and
`gojira.ErrConfigInvalidValue` are re-exported from
`internal/config`, so callers can `errors.Is` against them without
importing internal packages.

## 10. Anti-patterns (MUST NOT)

Agents MUST NOT:

- Issue raw `curl` (or any other HTTP client) calls against
  `https://<tenant>.atlassian.net/rest/api/3/...`.
- Ask the user to paste an API token into a prompt, plan, or
  comment.
- Store tokens, cookies, or session ids in chat context, prompts,
  or memory tools.
- Pass `--token` on a shared shell when an env var or `0600` config
  file is available — it leaks into shell history.
- Log Authorization headers, tokens, or credentials at any log
  level, including `--log-level trace`.
- Hard-code or guess transition ids or status names. Always run
  `gojira transitions` first.
- Call `gojira transition` without first listing.
- Hand-edit `<ISSUE-KEY>/index.md` and expect the next crawl to
  preserve the edits.
- Invent fields, custom field ids, statuses, RPCs, MCP tool names,
  or flag values that are not documented above.
- Use undocumented or browser-internal Jira endpoints without
  explicit user approval.
- Make live Jira calls in unit tests. Use fixtures and `httptest`
  fakes. Integration / E2E tests belong in the `integtest/`
  package at the module root.

If a Jira link cannot be resolved (permissions, rate limits,
deleted issue), the agent MUST surface the unresolved state rather
than invent content for it.

## 11. Quick reference

| Intent                            | MCP tool            | CLI command                                    |
| --------------------------------- | ------------------- | ---------------------------------------------- |
| Classify a key or URL             | `classify`          | (use crawl on the resolved key)                |
| Read one issue                    | `get_issue`         | `gojira crawl <KEY>` (writes to disk)          |
| Read an issue graph               | `crawl`             | `gojira crawl <KEY>`                           |
| Read graph in memory only         | `get_graph`         | (use MCP or gRPC)                              |
| List available transitions        | `list_transitions`  | `gojira transitions <KEY>`                     |
| Move an issue                     | `transition_issue`  | `gojira transition <KEY> --id <ID>`            |
| Move by status name               | `transition_issue`  | `gojira transition <KEY> --to-status <NAME>`   |
| Add a comment                     | `add_comment`       | `gojira comment <KEY> --text <T>`              |
| Create an issue                   | `create_issue`      | `gojira create --project <K> --summary <S>`    |
| Edit issue fields                 | `update_issue`      | `gojira update <KEY> --summary <S>`            |

Connection flags available on every write CLI command:
`--config`, `--site`, `--user`, `--token`. Prefer config file +
environment variables over `--token`.

## 12. References

- [`AGENTS.md`](../AGENTS.md) — top-level agent rules and package
  layout.
- [`.aider-desk/rules/20-jira-atlassian-api.md`](../.aider-desk/rules/20-jira-atlassian-api.md)
  — Atlassian API source-of-truth rules.
- [`.aider-desk/rules/40-agent-workflow-and-safety.md`](../.aider-desk/rules/40-agent-workflow-and-safety.md)
  — agent workflow, secrets, and test/network safety.
- [`.aider-desk/rules/50-app-config-and-cascade.md`](../.aider-desk/rules/50-app-config-and-cascade.md)
  — configuration cascade and validation contract.
- [`README.md`](../README.md) — `gojira mcp` and `gojira serve`
  sections.
