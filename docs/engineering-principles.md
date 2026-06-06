# gojira engineering principles

Small, durable rules the project follows. These complement the
project design doc (`jira-markdown-crawler-design.md`) and the PRD
by capturing **how** we write code, separately from **what** the
code does.

This file is for humans contributing to gojira. AI agents working
on the project read a copy of the same principles from
`.aider-desk/rules/10-go-engineering.md`. The two files must stay
in sync when one changes.

## Function signatures and parameterization

A function's signature must declare every parameter the function's
behavior depends on. If a value influences what the function does
at runtime, the caller passes it. Constants embedded in the
function body that *could* meaningfully differ between calls are
not allowed.

- If only one value is sensible today but multiple values are
  plausible tomorrow, declare the parameter today. Removing
  options later is easy; adding parameters later is a breaking
  change.
- Default values belong in an argument-default position (or in a
  `Config` / `Options` struct), not inside an `if`-statement deep
  in the function body that hard-codes the only blessed choice.
- Burying behavior-affecting constants inside a function body
  makes the public signature lie about the contract. Future
  reviewers and AI agents read the signature first; the body
  must agree with it.
- Where a parameter shape is uncertain at design time, prefer a
  typed enum, a string with a documented set of accepted values,
  or a sentinel like `""` / `nil` with explicit behavior in the
  doc comment. Do not stub it as a constant.

### Concrete examples this rule has caught

**Example 1 — `client.DevStatus`**

The first version of the Jira Dev Status client method had this
signature:

```go
func (c *Client) DevStatus(
    ctx context.Context, issueNumericID, application string,
) (DevStatusResponse, error)
```

Inside the body, the request URL hard-coded
`q.Set("dataType", "pullrequest")`. The upstream endpoint actually
accepts five `dataType` values (`pullrequest`, `branch`, `commit`,
`repository`, `build`). Users dogfooded an issue where the only
linked development entity was a repository, and gojira silently
missed it — the signature said "I fetch dev status" but the body
said "I only fetch pull requests."

The correct signature, used today:

```go
func (c *Client) DevStatus(
    ctx context.Context, issueNumericID, application, dataType string,
) (DevStatusResponse, error)
```

**Example 2 — section names in renderers**

A render helper that hard-coded a Markdown section heading inside
the body when the caller had every reason to pass it. The
hard-coded constant locked the function to one section even
though three other sections wanted to reuse it. Fixed by adding
a `sectionName string` parameter.

## Other engineering rules

See `.aider-desk/rules/10-go-engineering.md` for the full set
covering: design posture, package design, HTTP/API calls, error
handling, testing, formatting, and style. The AI-agent rules file
is the more frequently-updated version; significant rule changes
flow into this human-facing doc periodically.
