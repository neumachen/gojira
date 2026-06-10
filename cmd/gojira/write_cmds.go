// Write subcommands for the gojira CLI.
//
// This file implements the five facade-write commands — create, update,
// comment, transitions, transition — and the small `loadWriteConfig`
// helper they share. They all reuse the same file<env<flag
// configuration cascade as crawl and serve, so a single GOJIRA_*
// environment / YAML file works across every subcommand of the binary.
//
// No new capability is added here that the library does not already
// expose: each Action is a thin shell over the [gojira] facade
// (CreateIssue / UpdateIssue / AddComment / ListTransitions /
// TransitionIssue / TransitionIssueByStatus), plus the dry-run body
// builders (BuildCreateIssueBody / BuildUpdateIssueBody).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	cli "github.com/urfave/cli/v3"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// connFlags declares the connection flags every write subcommand
// accepts: --config / --site / --user / --token. They share the same
// env keys as crawl/serve so a single configured environment drives
// every subcommand uniformly. These are the only flags
// [loadWriteConfig] feeds into the config cascade for write commands.
func connFlags(env map[string]string) []cli.Flag {
	src := func(key string) cli.ValueSourceChain {
		return cli.NewValueSourceChain(newMapValueSource(env, key))
	}
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "config",
			Usage:   "Path to YAML config file (overrides discovery)",
			Sources: src("GOJIRA_CONFIG_FILE"),
		},
		&cli.StringFlag{
			Name:    "site",
			Usage:   "Jira Cloud base URL",
			Sources: src("GOJIRA_SITE"),
		},
		&cli.StringFlag{
			Name:    "user",
			Usage:   "Atlassian account email",
			Sources: src("GOJIRA_USER"),
		},
		&cli.StringFlag{
			Name:    "token",
			Usage:   "Atlassian API token",
			Sources: src("GOJIRA_TOKEN"),
		},
	}
}

// loadWriteConfig runs the same file<env<flag cascade as runCrawl /
// runServe, driven by the write subcommand's connection flags.
//
// Write commands do not need a real output directory, but the legacy
// gojira.LoadConfig validator still requires GOJIRA_OUTPUT_DIR to be
// non-empty (the field exists on the shared [gojira.Config] struct
// used across read and write paths). When none has been configured we
// fall back to a sentinel placeholder ("." — the working directory)
// so the validator passes without forcing every CLI user to set an
// output directory they will never use.
func loadWriteConfig(cmd *cli.Command, env map[string]string, stderr io.Writer) (gojira.Config, error) {
	configPath := cmd.String("config")

	fileCfg, err := gojira.LoadFileConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return gojira.Config{}, &exitErr{code: 1, msg: "config", wrap: err}
	}

	mergedKV := configToKV(fileCfg)
	for k, v := range config.ResolveAliases(env) {
		if v != "" {
			mergedKV[k] = v
		}
	}
	// buildConfigKV checks cmd.IsSet for every crawl flag; the write
	// commands only declare a subset of those flags, but IsSet returns
	// false for any undeclared flag (no panic), so reusing it is safe.
	for k, v := range buildConfigKV(cmd) {
		mergedKV[k] = v
	}

	// Fall back to "." for an unset OUTPUT_DIR so legacy LoadConfig's
	// required-field validator passes. Write commands never read or
	// write the field, so the sentinel value never escapes the cascade.
	if mergedKV["GOJIRA_OUTPUT_DIR"] == "" {
		mergedKV["GOJIRA_OUTPUT_DIR"] = "."
	}

	cfg, err := gojira.LoadConfig(mergedKV)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return gojira.Config{}, &exitErr{code: 1, msg: "config", wrap: err}
	}
	return cfg, nil
}

// requireOneKey enforces "exactly one positional argument" with the
// same UX as runCrawl: a clear stderr error and an *exitErr{code:1}.
func requireOneKey(cmd *cli.Command, stderr io.Writer) (string, error) {
	positional := cmd.Args().Slice()
	if len(positional) == 0 {
		fmt.Fprintf(stderr, "error: missing required argument <ISSUE-KEY>\n")
		return "", &exitErr{code: 1, msg: "missing <ISSUE-KEY>"}
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "error: too many arguments (expected one <ISSUE-KEY>, got %d)\n", len(positional))
		return "", &exitErr{code: 1, msg: "too many arguments"}
	}
	return positional[0], nil
}

// stderrOf returns the cmd's resolved stderr writer, falling back to
// os.Stderr when the root has none — mirroring runCrawl / runServe.
func stderrOf(cmd *cli.Command) io.Writer {
	w := cmd.Root().ErrWriter
	if w == nil {
		return os.Stderr
	}
	return w
}

// stdoutOf returns the cmd's resolved stdout writer, falling back to
// os.Stdout when the root has none.
func stdoutOf(cmd *cli.Command) io.Writer {
	w := cmd.Root().Writer
	if w == nil {
		return os.Stdout
	}
	return w
}

// prettyJSON pretty-prints raw JSON. It is used by --dry-run output so
// the body the CLI would have posted is human-readable in the
// terminal. A round-trip through json.Indent preserves the original
// field order produced by the client renderer.
func prettyJSON(raw []byte) []byte {
	var out []byte
	var any json.RawMessage = raw
	pretty, err := json.MarshalIndent(any, "", "  ")
	if err != nil {
		// Fall back to the raw bytes; the renderer is supposed to
		// produce valid JSON, but if a future change breaks that we
		// still print *something* useful rather than nothing.
		return append(out, raw...)
	}
	return pretty
}

// printAPIError prints a *client.APIError's field-level errors to
// stderr in a compact, human-friendly form. The APIError.Error()
// string already contains the same information inline (see
// client/errors.go), but pulling the FieldErrors out into a list
// is easier to scan when there are several. The caller is still
// responsible for the top-level "error:" prefix.
func printAPIError(stderr io.Writer, err error) {
	var ape *client.APIError
	if !errors.As(err, &ape) || len(ape.FieldErrors) == 0 {
		return
	}
	fmt.Fprintln(stderr, "Jira field errors:")
	for k, v := range ape.FieldErrors {
		fmt.Fprintf(stderr, "  %s: %s\n", k, v)
	}
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func createCommand(env map[string]string) *cli.Command {
	conn := connFlags(env)
	flags := append(conn,
		&cli.StringFlag{Name: "project", Usage: "Project key (required)"},
		&cli.StringFlag{Name: "type", Usage: "Issue type", Value: "Task"},
		&cli.StringFlag{Name: "summary", Usage: "Issue summary (required)"},
		&cli.StringFlag{Name: "description", Usage: "Issue description (plain text, converted to ADF)"},
		&cli.StringFlag{Name: "assignee", Usage: "Assignee accountId"},
		&cli.StringSliceFlag{Name: "label", Usage: "Issue label (repeatable)"},
		&cli.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be POSTed and exit (no HTTP call)"},
	)
	return &cli.Command{
		Name:  "create",
		Usage: "Create a new Jira issue",
		Flags: flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runCreate(ctx, cmd, env)
		},
	}
}

func runCreate(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	project := cmd.String("project")
	if project == "" {
		fmt.Fprintln(stderr, "error: --project is required")
		return &exitErr{code: 1, msg: "missing --project"}
	}
	summary := cmd.String("summary")
	if summary == "" {
		fmt.Fprintln(stderr, "error: --summary is required")
		return &exitErr{code: 1, msg: "missing --summary"}
	}
	issueType := cmd.String("type")
	if issueType == "" {
		issueType = "Task"
	}

	opts := []client.CreateOption{client.WithSummary(summary)}
	if desc := cmd.String("description"); desc != "" {
		opts = append(opts, client.WithDescriptionText(desc))
	}
	if a := cmd.String("assignee"); a != "" {
		opts = append(opts, client.WithAssigneeAccountID(a))
	}
	if labels := cmd.StringSlice("label"); len(labels) > 0 {
		opts = append(opts, client.WithLabels(labels...))
	}

	if cmd.Bool("dry-run") {
		body, err := gojira.BuildCreateIssueBody(project, issueType, opts...)
		if err != nil {
			fmt.Fprintf(stderr, "error: build create body: %v\n", err)
			return &exitErr{code: 1, msg: "build create body", wrap: err}
		}
		fmt.Fprintln(stdout, string(prettyJSON(body)))
		return nil
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	res, err := gojira.CreateIssue(ctx, cfg, project, issueType, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "error: create issue: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "create issue", wrap: err}
	}
	fmt.Fprintf(stdout, "Created %s (id %s)\n%s\n", res.Key, res.ID, res.Self)
	return nil
}

// ---------------------------------------------------------------------------
// update
// ---------------------------------------------------------------------------

func updateCommand(env map[string]string) *cli.Command {
	conn := connFlags(env)
	flags := append(conn,
		&cli.StringFlag{Name: "summary", Usage: "New summary"},
		&cli.StringFlag{Name: "description", Usage: "New description (plain text)"},
		&cli.StringFlag{Name: "assignee", Usage: "New assignee accountId"},
		&cli.StringSliceFlag{Name: "label", Usage: "Replacement label (repeatable; replaces, does not append)"},
		&cli.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be PUT and exit (no HTTP call)"},
	)
	return &cli.Command{
		Name:      "update",
		Usage:     "Edit fields on an existing Jira issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runUpdate(ctx, cmd, env)
		},
	}
}

func runUpdate(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	key, err := requireOneKey(cmd, stderr)
	if err != nil {
		return err
	}

	// Build []UpdateOption only for flags the user actually set, so
	// unset flags do NOT overwrite existing field values with empty
	// strings.
	var opts []client.UpdateOption
	if cmd.IsSet("summary") {
		opts = append(opts, client.WithSummaryUpdate(cmd.String("summary")))
	}
	if cmd.IsSet("description") {
		opts = append(opts, client.WithDescriptionTextUpdate(cmd.String("description")))
	}
	if cmd.IsSet("assignee") {
		opts = append(opts, client.WithAssigneeAccountIDUpdate(cmd.String("assignee")))
	}
	if cmd.IsSet("label") {
		opts = append(opts, client.WithLabelsUpdate(cmd.StringSlice("label")...))
	}

	if len(opts) == 0 {
		fmt.Fprintln(stderr, "error: nothing to update — pass at least one of --summary/--description/--assignee/--label")
		return &exitErr{code: 1, msg: "nothing to update"}
	}

	if cmd.Bool("dry-run") {
		body, err := gojira.BuildUpdateIssueBody(opts...)
		if err != nil {
			fmt.Fprintf(stderr, "error: build update body: %v\n", err)
			return &exitErr{code: 1, msg: "build update body", wrap: err}
		}
		fmt.Fprintln(stdout, string(prettyJSON(body)))
		return nil
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	if err := gojira.UpdateIssue(ctx, cfg, key, opts...); err != nil {
		fmt.Fprintf(stderr, "error: update issue: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "update issue", wrap: err}
	}
	fmt.Fprintf(stdout, "Updated %s\n", key)
	return nil
}

// ---------------------------------------------------------------------------
// comment
// ---------------------------------------------------------------------------

func commentCommand(env map[string]string) *cli.Command {
	conn := connFlags(env)
	flags := append(conn,
		&cli.StringFlag{Name: "text", Usage: "Comment body (required, plain text)"},
	)
	return &cli.Command{
		Name:      "comment",
		Usage:     "Add a comment to a Jira issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runComment(ctx, cmd, env)
		},
	}
}

func runComment(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	key, err := requireOneKey(cmd, stderr)
	if err != nil {
		return err
	}
	text := cmd.String("text")
	if text == "" {
		fmt.Fprintln(stderr, "error: --text is required")
		return &exitErr{code: 1, msg: "missing --text"}
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	c, err := gojira.AddComment(ctx, cfg, key, client.WithCommentText(text))
	if err != nil {
		fmt.Fprintf(stderr, "error: add comment: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "add comment", wrap: err}
	}
	fmt.Fprintf(stdout, "Added comment %s to %s\n", c.ID, key)
	return nil
}

// ---------------------------------------------------------------------------
// transitions (list)
// ---------------------------------------------------------------------------

func transitionsCommand(env map[string]string) *cli.Command {
	return &cli.Command{
		Name:      "transitions",
		Usage:     "List the workflow transitions currently available for an issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     connFlags(env),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runTransitions(ctx, cmd, env)
		},
	}
}

func runTransitions(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	key, err := requireOneKey(cmd, stderr)
	if err != nil {
		return err
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	ts, err := gojira.ListTransitions(ctx, cfg, key)
	if err != nil {
		fmt.Fprintf(stderr, "error: list transitions: %v\n", err)
		return &exitErr{code: 1, msg: "list transitions", wrap: err}
	}
	if len(ts) == 0 {
		fmt.Fprintf(stdout, "No transitions available for %s\n", key)
		return nil
	}
	for _, t := range ts {
		fmt.Fprintf(stdout, "%s\t%s\t-> %s\n", t.ID, t.Name, t.ToStatus)
	}
	return nil
}

// ---------------------------------------------------------------------------
// transition (execute)
// ---------------------------------------------------------------------------

func transitionCommand(env map[string]string) *cli.Command {
	conn := connFlags(env)
	flags := append(conn,
		&cli.StringFlag{Name: "id", Usage: "Transition id (mutually exclusive with --to-status)"},
		&cli.StringFlag{Name: "to-status", Usage: "Target status name to resolve server-side (mutually exclusive with --id)"},
		&cli.StringFlag{Name: "comment", Usage: "Optional comment to add during the transition"},
	)
	return &cli.Command{
		Name:      "transition",
		Usage:     "Move an issue through a workflow transition",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runTransition(ctx, cmd, env)
		},
	}
}

func runTransition(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	key, err := requireOneKey(cmd, stderr)
	if err != nil {
		return err
	}

	id := cmd.String("id")
	toStatus := cmd.String("to-status")
	switch {
	case id != "" && toStatus != "":
		fmt.Fprintln(stderr, "error: pass exactly one of --id or --to-status, not both")
		return &exitErr{code: 1, msg: "both --id and --to-status set"}
	case id == "" && toStatus == "":
		fmt.Fprintln(stderr, "error: pass exactly one of --id or --to-status")
		return &exitErr{code: 1, msg: "neither --id nor --to-status set"}
	}

	var topts []client.TransitionOption
	if c := cmd.String("comment"); c != "" {
		topts = append(topts, client.WithTransitionCommentText(c))
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	if id != "" {
		if err := gojira.TransitionIssue(ctx, cfg, key, id, topts...); err != nil {
			fmt.Fprintf(stderr, "error: transition issue: %v\n", err)
			printAPIError(stderr, err)
			return &exitErr{code: 1, msg: "transition issue", wrap: err}
		}
		fmt.Fprintf(stdout, "Transitioned %s via transition id %s\n", key, id)
		return nil
	}

	if err := gojira.TransitionIssueByStatus(ctx, cfg, key, toStatus, topts...); err != nil {
		fmt.Fprintf(stderr, "error: transition issue: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "transition issue", wrap: err}
	}
	fmt.Fprintf(stdout, "Transitioned %s to %q\n", key, toStatus)
	return nil
}
