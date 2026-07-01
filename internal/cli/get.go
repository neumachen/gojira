// Get subcommand for the gojira CLI.
//
// This file implements the `gojira get <ISSUE-KEY>` command, which fetches
// a single Jira issue (no recursion, no crawl, no disk writes) and prints
// it to stdout. Default output is Markdown; --format json prints structured
// issue+references JSON. The command does NOT require GOJIRA_OUTPUT_DIR.
package cli

import (
	"context"
	"fmt"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/render"
	urfave "github.com/urfave/cli/v3"
)

// ---------------------------------------------------------------------------
// get command
// ---------------------------------------------------------------------------

// getCommand returns the *urfave.Command for "gojira get".
func getCommand(env map[string]string) *urfave.Command {
	flags := append(connFlags(env),
		&urfave.StringFlag{
			Name:  "format",
			Usage: "Output format: markdown|json",
			Value: "markdown",
		},
	)
	return &urfave.Command{
		Name:      "get",
		Usage:     "Fetch a single Jira issue and print it (no crawl, no files written)",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runGet(ctx, cmd, env)
		},
	}
}

// runGet is the body of the "gojira get" subcommand. It fetches a single
// Jira issue and prints either Markdown or JSON to stdout, depending on
// the --format flag. It does NOT write any files to disk.
func runGet(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
	stdout := stdoutOf(cmd)
	stderr := stderrOf(cmd)

	// Pre-flight config guard.
	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	// Exactly one positional argument: <ISSUE-KEY>.
	key, err := requireOneKey(cmd, stderr)
	if err != nil {
		return err
	}

	// Parse --format flag.
	formatStr := cmd.String("format")
	format, err := gojira.ParseOutputFormat(formatStr)
	if err != nil {
		fmt.Fprintf(stderr, "error: invalid format %q\n", formatStr)
		return &exitErr{code: 1, msg: "invalid format", wrap: err}
	}

	// Reject FormatStructured — only markdown/json make sense for stdout.
	if format == gojira.FormatStructured {
		fmt.Fprintln(stderr, "error: structured format is not supported for stdout; use markdown or json")
		return &exitErr{code: 1, msg: "structured format not supported"}
	}

	// Load config via the shared cascade (loadWriteConfig injects a "."
	// sentinel for output-dir, so this works without GOJIRA_OUTPUT_DIR).
	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	// Fetch and render based on format.
	switch format {
	case gojira.FormatMarkdown:
		indexMD, _, _, fetchErr := gojira.FetchAndRender(ctx, cfg, key)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "error: get issue: %v\n", fetchErr)
			return &exitErr{code: 1, msg: "get issue", wrap: fetchErr}
		}
		fmt.Fprintln(stdout, indexMD)

	case gojira.FormatJSON:
		issue, refs, fetchErr := gojira.GetIssue(ctx, cfg, key)
		if fetchErr != nil {
			fmt.Fprintf(stderr, "error: get issue: %v\n", fetchErr)
			return &exitErr{code: 1, msg: "get issue", wrap: fetchErr}
		}
		j, renderErr := render.RenderIssueJSON(issue, refs)
		if renderErr != nil {
			fmt.Fprintf(stderr, "error: render json: %v\n", renderErr)
			return &exitErr{code: 1, msg: "render json", wrap: renderErr}
		}
		fmt.Fprintln(stdout, j)
	}

	return nil
}
