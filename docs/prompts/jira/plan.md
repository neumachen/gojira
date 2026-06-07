# `/jira/plan "<slice>"` — inspect and plan, no edits

Use this command when you have an idea for a small slice of work but
don't yet know which files it touches or how to break it up. The
output is a plan; no source files are modified.

## Inputs

- `<slice>`: a one- or two-sentence description of the change. Be
  concrete about the user-visible outcome (a CLI flag, a new
  exported function, a bug fix) and the package(s) you suspect are
  involved.

## What to produce

1. **Codebase recon.** Identify the relevant packages, the file the
   change starts in, and any cross-cutting concerns the slice will
   touch (rendering, config, client, crawl, output). Surface package
   boundaries and existing conventions.
2. **Files to change.** A short bulleted list with one line per file
   explaining the intended edit; mark new files as such.
3. **Tests to add or update.** Name the test files and the table-driven
   cases the slice needs (TDD-first; failing test names go here).
4. **Approach.** One paragraph: the intended sequence (RED → GREEN
   → REFACTOR), the data structures involved, and any external
   contract that must be preserved (e.g. exit codes, error
   sentinels, the `App.ToConfig()` compatibility invariant).
5. **Risks and unknowns.** Anything you'd need to ask the user about
   before implementing — third-party deps, schema bumps, behavior
   changes to existing flags.

## Constraints

- Do NOT edit code, tests, configs, or docs in this command.
- Do NOT install dependencies or modify `go.mod`.
- Do NOT run long shell commands; reading the repo is enough.
- Respect [`.aider-desk/rules/`](../../../.aider-desk/rules/) —
  especially `10-go-engineering.md`,
  `40-agent-workflow-and-safety.md`, and
  `50-app-config-and-cascade.md` when the slice touches
  configuration.
- The plan should fit on one screen. If the slice is too large for
  that, split it into multiple slices in your reply rather than
  producing a sprawling plan.

## Next step

When the user approves the plan, hand off to `/jira/implement-slice`.
