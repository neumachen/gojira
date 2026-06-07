# `/jira/test` — run and summarise the project checks

Run the project's standard quality gates and produce a concise
pass/fail report. Use this command to verify the working tree is
green before a commit, after a refactor, or as part of `/jira/review-diff`.

## What to run

In this order; STOP on the first hard failure unless the user asks
for full results:

1. `gofmt -l .` — must be empty for tracked Go files. Any output is
   a failure.
2. `go vet ./...` — must be silent. Any output is a failure.
3. `bash scripts/aider-lint-go.sh` — wraps the lint pipeline the
   repo standardises on. Exit 0 required.
4. `bash scripts/aider-test.sh` — wraps the test pipeline. Exit 0
   required.
5. `go test ./...` — final belt-and-suspenders run with a
   per-package status line.

## Constraints

- Hermetic: no live network, no real credentials. The repo's tests
  are designed to run offline; if a test tries to contact the
  internet, that is a bug in the test, not the network.
- Quick: keep total wall-clock under a couple of minutes on a
  developer laptop; long-running tests should be opt-in via build
  tag, not the default suite.
- Honest: do NOT swallow failures or paper over flakes. If a test
  is flaky, surface the flake; do not "rerun until green".

## Output format

```text
## Quick checks
- gofmt: OK | FAIL (<paths>)
- go vet: OK | FAIL (<first error>)

## Lint
- scripts/aider-lint-go.sh: OK | FAIL (exit <code>)

## Tests
- scripts/aider-test.sh: OK | FAIL (exit <code>)
- go test ./...: OK | FAIL (<package: short reason>)
  ok  	github.com/neumachen/gojira	<time>
  ok  	github.com/neumachen/gojira/classify	<time>
  ...

## Verdict
GREEN | RED — <one-line summary>
```

If any step fails, include the first ~20 lines of the failing
output so the user can act on it directly without rerunning the
command.
