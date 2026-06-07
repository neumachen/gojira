# `/jira/review-diff` — review the current uncommitted diff

Review every uncommitted change in the working tree (`git status` +
`git diff`) against the project's engineering rules and the gojira
configuration architecture. Output a structured review.

## What to inspect

1. `git status --short` — what's staged, what's unstaged, what's
   untracked. Flag any unexpected file paths (especially anything
   under `.aider-desk/` unless explicitly intended).
2. `git diff` (unstaged) and `git diff --cached` (staged) — read the
   actual diff line by line, not just the file list.
3. New test files: confirm they fail for the right reasons before
   the implementation, that they use `testify`, and that they avoid
   live network calls.
4. New source files: check error handling (every error checked),
   `context.Context` propagation on external calls, idiomatic Go
   naming, and absence of secrets.

## Review criteria

Score the diff on each of the following dimensions and surface
findings under each heading:

- **Correctness** — Does the diff do what its message claims? Edge
  cases covered? Regressions in existing tests?
- **Go idioms** — Naming, package boundaries, error wrapping with
  `errext` / `fmt.Errorf("%w", ...)`, slog use, no panics in
  library code.
- **Test coverage** — New behavior tested? Failure paths covered?
  Table-driven where appropriate? `t.Cleanup` / `t.TempDir` /
  `t.Chdir` used so tests don't leak state?
- **Configuration architecture** — If the diff touches config:
  does it follow `.aider-desk/rules/50-app-config-and-cascade.md`?
  Embedded default + JSON Schema + `gojira.example.yaml` +
  `App.ToConfig` + alias (if v0.1 predecessor exists) + Layer-2
  validation + README env-var table — which of these are missing?
- **Compatibility invariant** — Does the diff modify
  `internal/config/config.go`? Does it make `client`, `crawl`,
  `fetch`, or the facade depend on `App` directly? Does it change
  CLI exit codes or the format of user-facing error messages?
  All three are red flags.
- **Security / secrets** — No tokens, credentials, hostnames, or
  PII committed. Logging does not surface auth headers.

## Output format

Use this structure so the response is easy to skim:

```text
## Summary
<one-sentence verdict: ready / changes requested / blocked>

## Blockers
- ...

## Concerns (must fix before merging)
- ...

## Suggestions (nice to have)
- ...

## Verification
- gofmt: <result>
- go vet: <result>
- scripts/aider-test.sh: <result>
- go test ./...: <tail summary>
```

If the diff is empty, say so and exit early; do not invent findings.
