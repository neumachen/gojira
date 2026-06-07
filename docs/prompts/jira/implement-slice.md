# `/jira/implement-slice "<slice>"` — implement a small tested vertical slice

Implement ONE small vertical slice end-to-end with tests, then stop.
Resist scope creep; if the slice grows mid-implementation, finish what
you have and surface the remainder as a follow-up.

## Inputs

- `<slice>`: the single, small outcome to deliver. If a plan was
  produced by `/jira/plan`, treat that plan as the authoritative
  files-to-change list.

## Required workflow (TDD)

1. **RED.** Write the failing test(s) first. Use `testify`
   (`assert`/`require`) and table-driven tests where they fit
   naturally. Run the test to confirm it FAILS for the documented
   reason — not for an unrelated reason (compile error, panic in
   setup, etc.).
2. **GREEN.** Implement the minimum code that makes the test pass.
   Keep new exported surface tight; prefer internal helpers.
3. **REFACTOR.** Tidy the diff. Names, comments, error wrapping,
   gofmt. Re-run tests to confirm still green.
4. **Verify the whole repo.** Run `scripts/aider-lint-go.sh`,
   `scripts/aider-test.sh`, and `go test ./...`. All must be green.

## Invariants you MUST preserve

- **Do not modify `internal/config/config.go`.** It is the
  authoritative flat `Config` type read by downstream consumers; the
  app-config refactor flattens onto it via `App.ToConfig()`.
- **Do not touch the crawl/fetch/client/facade code path** for a
  configuration change — the `App.ToConfig()` bridge means new fields
  flow downstream automatically once they have a `Config` slot.
- Preserve `errors.Is(err, gojira.ErrConfigMissingRequired)` and
  `errors.Is(err, gojira.ErrConfigInvalidValue)` for any new
  config-validation failure.
- Preserve CLI exit codes (0 / 1 / 2 / 130) and the format of
  user-facing error messages downstream tests assert on.

## Working rules

- Idiomatic Go; small cohesive packages; explicit error handling
  (`check every error`).
- `context.Context` on every external operation.
- No new third-party dependency without asking the user (see
  `40-agent-workflow-and-safety.md`).
- No secrets, no credentials, no live network calls in tests.
- Honour the configuration architecture documented in
  `.aider-desk/rules/50-app-config-and-cascade.md` when the slice
  adds or modifies a config field (entity default, JSON Schema
  property, `App.ToConfig` mapping, alias entry if applicable,
  README env-var table update, `gojira.example.yaml` update).

## Output

- A short summary of what changed (files touched, tests added).
- The exact `go test ./...` tail showing all packages green.
- The commit message you propose, in conventional-commit form
  (`feat(<area>): <short>` or `fix(<area>): <short>` plus a brief
  body). Do not push.
