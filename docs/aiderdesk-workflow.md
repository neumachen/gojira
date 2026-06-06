# AiderDesk workflow for gojira

This document describes the recommended Aider/AiderDesk setup for implementing `gojira`, a Go Jira-to-Markdown crawler.

## Repository configuration

Project-level Aider config:

```text
.aider.conf.yml
```

AiderDesk project rules:

```text
.aider-desk/rules/
```

AiderDesk custom commands:

```text
.aider-desk/commands/jira/
```

Prompt archive:

```text
docs/prompts/
```

Agent guidance:

```text
AGENTS.md
.aider-desk/agents/jira-go-implementer/rules/implementer.md
.aider-desk/agents/jira-go-reviewer/rules/reviewer.md
```

## Recommended AiderDesk agent profiles

The exact AiderDesk profile JSON schema was not generated here. Configure these profiles in the AiderDesk UI if the local version supports them.

### `jira-go-implementer`

Purpose: repository discovery and small code edits.

Recommended settings:

- Base/profile type: Aider-enabled coding agent, preferably with repository search.
- Temperature: low/deterministic.
- Max iterations: moderate; enough for multi-file Go changes, not unbounded.
- Tools:
  - allow repository/file read tools,
  - allow Aider code-edit tools,
  - allow test execution after confirmation,
  - allow Power Search/Power Tools if available,
  - allow Todo tools if available.
- Approval posture:
  - auto-approve read-only repository inspection if you are comfortable,
  - ask before file writes if not already operating in an approved implementation command,
  - ask before shell commands that modify files,
  - ask before dependency changes,
  - ask before network calls,
  - ask before git operations.

Use this profile with:

```text
/jira/plan "<feature slice>"
/jira/implement-slice "<feature slice>"
```

### `jira-go-reviewer`

Purpose: read-only or mostly read-only review of uncommitted changes.

Recommended settings:

- Temperature: low.
- Prefer read-only file and diff inspection.
- Allow test execution if approved.
- Do not edit code unless explicitly asked.

Use this profile with:

```text
/jira/review-diff
/jira/test
```

### Optional `jira-api-researcher`

Purpose: verify Atlassian/Jira API behavior against official docs.

Recommended settings:

- Web/docs access enabled.
- Restrict sources to official Atlassian documentation when possible.
- Do not edit code directly.
- Summarize verified facts, assumptions, and uncertainties.

Useful when deciding how to handle:

- Jira Cloud REST API v3 endpoints.
- Jira Software development metadata.
- Remote links.
- ADF schema details.
- Rate limits.
- Tenant-specific field metadata.

### Optional `test-runner`

Purpose: run checks and summarize failures.

Recommended settings:

- Shell/test execution allowed with approval.
- No code edits unless explicitly requested.

## Custom commands

### `/jira/plan "<slice>"`

Use this before implementation. It asks AiderDesk to inspect the repository, identify the smallest useful vertical slice, and produce a plan.

Example:

```text
/jira/plan "Implement URL classification for Jira issue links, non-Jira links, and GitHub pull request links"
```

### `/jira/implement-slice "<slice>"`

Use this for small implementation increments.

Example:

```text
/jira/implement-slice "Implement URL classification for Jira issue links, non-Jira links, and GitHub pull request links with table-driven tests"
```

### `/jira/review-diff`

Use this after changes.

Example:

```text
/jira/review-diff "Focus on link classification correctness and missing edge cases"
```

### `/jira/test`

Use this to run and summarize checks.

Example:

```text
/jira/test
```

## Hooks/checks

Configured in `.aider.conf.yml`:

- `auto-lint: true`
- `lint-cmd` uses `./scripts/aider-lint-go.sh`
- `test-cmd` uses `./scripts/aider-test.sh`
- `auto-test: false` initially, so tests are deliberate rather than automatic on every change

## Recommended implementation sequence

1. Design document
   - Create `docs/jira-markdown-crawler-design.md` using `docs/prompts/jira-crawler-design-aiderdesk.md`.

2. Pure link classification
   - Jira issue key recognition.
   - Jira issue URL recognition.
   - GitHub pull request URL recognition.
   - Non-Jira URL classification.

3. Markdown path/reference rendering
   - `<ISSUE-KEY>/index.md` paths.
   - `<ISSUE-KEY>/references/` support.
   - Relative links between issue pages.

4. Crawl state
   - Queue.
   - Visited set.
   - Deduplication.
   - Configured limits.

5. ADF traversal
   - Extract links from ADF nodes and link marks.
   - Preserve unsupported nodes safely.

6. Jira client skeleton
   - Context-aware HTTP client.
   - Auth injection.
   - Issue fetch abstraction.
   - Pagination/rate-limit behavior.

7. Rendering
   - Markdown issue page structure.
   - Relationship sections.
   - Pull request references.
   - Unresolved links.
   - Golden tests.

8. CLI
   - Config parsing.
   - Start issue keys.
   - Output directory.
   - Crawl limits.

9. Integration tests
   - Mock server-based tests.
   - Optional live integration tests gated by environment variables.

## Safety rules

- Do not commit secrets.
- Do not make live Jira/GitHub calls in unit tests.
- Do not use undocumented Jira browser APIs without approval.
- Do not add dependencies without explaining why.
- Do not change the canonical Markdown layout without approval.
- Do not recursively crawl non-Jira links by default.

## Useful next prompt

Start with:

```text
/jira/plan "Create the initial docs/jira-markdown-crawler-design.md design document using docs/prompts/jira-crawler-design-aiderdesk.md as the source prompt. Do not implement code yet."
```

Then:

```text
/jira/implement-slice "Implement URL classification for Jira issue links, non-Jira links, and GitHub pull request links with table-driven tests. Keep this pure and avoid network calls."
```
