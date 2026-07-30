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
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/pkg/client"
	urfave "github.com/urfave/cli/v3"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// connFlags declares the connection flags every write subcommand
// accepts: --config / --site / --user / --token. They share the same
// env keys as crawl/serve so a single configured environment drives
// every subcommand uniformly. These are the only flags
// [loadWriteConfig] feeds into the config cascade for write commands.
func connFlags(env map[string]string) []urfave.Flag {
	src := func(key string) urfave.ValueSourceChain {
		return urfave.NewValueSourceChain(newMapValueSource(env, key))
	}
	return []urfave.Flag{
		&urfave.StringFlag{
			Name:    "config",
			Usage:   "Path to YAML config file (overrides discovery)",
			Sources: src("GOJIRA_CONFIG_FILE"),
		},
		&urfave.StringFlag{
			Name:    "site",
			Usage:   "Jira Cloud base URL",
			Sources: src("GOJIRA_SITE"),
		},
		&urfave.StringFlag{
			Name:    "user",
			Usage:   "Atlassian account email",
			Sources: src("GOJIRA_USER"),
		},
		&urfave.StringFlag{
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
func loadWriteConfig(cmd *urfave.Command, env map[string]string, stderr io.Writer) (gojira.Config, error) {
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
func requireOneKey(cmd *urfave.Command, stderr io.Writer) (string, error) {
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
func stderrOf(cmd *urfave.Command) io.Writer {
	w := cmd.Root().ErrWriter
	if w == nil {
		return os.Stderr
	}
	return w
}

// stdoutOf returns the cmd's resolved stdout writer, falling back to
// os.Stdout when the root has none.
func stdoutOf(cmd *urfave.Command) io.Writer {
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

func createCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "project", Usage: "Project key (required)"},
		&urfave.StringFlag{Name: "type", Usage: "Issue type", Value: "Task"},
		&urfave.StringFlag{Name: "summary", Usage: "Issue summary (required)"},
		&urfave.StringFlag{Name: "description", Usage: "Issue description (plain text, converted to ADF)"},
		&urfave.StringFlag{Name: "assignee", Usage: "Assignee accountId"},
		&urfave.StringSliceFlag{Name: "label", Usage: "Issue label (repeatable)"},
		&urfave.StringSliceFlag{Name: "fix-version", Usage: "Fix version by name (repeatable)"},
		&urfave.StringSliceFlag{Name: "fix-version-id", Usage: "Fix version by id (repeatable)"},
		&urfave.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be POSTed and exit (no HTTP call)"},
	)
	return &urfave.Command{
		Name:  "create",
		Usage: "Create a new Jira issue",
		Flags: flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runCreate(ctx, cmd, env)
		},
	}
}

func runCreate(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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
	if names := cmd.StringSlice("fix-version"); len(names) > 0 {
		opts = append(opts, client.WithFixVersionNames(names...))
	}
	if ids := cmd.StringSlice("fix-version-id"); len(ids) > 0 {
		opts = append(opts, client.WithFixVersionIDs(ids...))
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

func updateCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "summary", Usage: "New summary"},
		&urfave.StringFlag{Name: "description", Usage: "New description (plain text)"},
		&urfave.StringFlag{Name: "assignee", Usage: "New assignee accountId"},
		&urfave.StringSliceFlag{Name: "label", Usage: "Replacement label (repeatable; replaces, does not append)"},
		&urfave.StringSliceFlag{Name: "fix-version", Usage: "Replacement fix version by name (repeatable; set-replace)"},
		&urfave.StringSliceFlag{Name: "fix-version-id", Usage: "Replacement fix version by id (repeatable; set-replace)"},
		&urfave.StringSliceFlag{Name: "add-fix-version", Usage: "Add a fix version by id (repeatable; incremental)"},
		&urfave.StringSliceFlag{Name: "remove-fix-version", Usage: "Remove a fix version by id (repeatable; incremental)"},
		&urfave.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be PUT and exit (no HTTP call)"},
	)
	return &urfave.Command{
		Name:      "update",
		Usage:     "Edit fields on an existing Jira issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runUpdate(ctx, cmd, env)
		},
	}
}

func runUpdate(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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
	if cmd.IsSet("fix-version") {
		opts = append(opts, client.WithFixVersionNamesUpdate(cmd.StringSlice("fix-version")...))
	}
	if cmd.IsSet("fix-version-id") {
		opts = append(opts, client.WithFixVersionIDsUpdate(cmd.StringSlice("fix-version-id")...))
	}
	if cmd.IsSet("add-fix-version") {
		for _, id := range cmd.StringSlice("add-fix-version") {
			opts = append(opts, client.WithFixVersionAdd(id))
		}
	}
	if cmd.IsSet("remove-fix-version") {
		for _, id := range cmd.StringSlice("remove-fix-version") {
			opts = append(opts, client.WithFixVersionRemove(id))
		}
	}

	if len(opts) == 0 {
		fmt.Fprintln(stderr, "error: nothing to update — pass at least one of --summary/--description/--assignee/--label/--fix-version/--fix-version-id/--add-fix-version/--remove-fix-version")
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

func commentCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "text", Usage: "Comment body (required, plain text)"},
	)
	return &urfave.Command{
		Name:      "comment",
		Usage:     "Add a comment to a Jira issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runComment(ctx, cmd, env)
		},
	}
}

func runComment(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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

func transitionsCommand(env map[string]string) *urfave.Command {
	return &urfave.Command{
		Name:      "transitions",
		Usage:     "List the workflow transitions currently available for an issue",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     connFlags(env),
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runTransitions(ctx, cmd, env)
		},
	}
}

func runTransitions(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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

func transitionCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "id", Usage: "Transition id (mutually exclusive with --to-status)"},
		&urfave.StringFlag{Name: "to-status", Usage: "Target status name to resolve server-side (mutually exclusive with --id)"},
		&urfave.StringFlag{Name: "comment", Usage: "Optional comment to add during the transition"},
	)
	return &urfave.Command{
		Name:      "transition",
		Usage:     "Move an issue through a workflow transition",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runTransition(ctx, cmd, env)
		},
	}
}

func runTransition(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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

// ---------------------------------------------------------------------------
// release (version management)
// ---------------------------------------------------------------------------

// requireOneVersionID enforces "exactly one positional argument" with the
// same UX as [requireOneKey], but labels the argument <VERSION-ID>.
func requireOneVersionID(cmd *urfave.Command, stderr io.Writer) (string, error) {
	positional := cmd.Args().Slice()
	if len(positional) == 0 {
		fmt.Fprintf(stderr, "error: missing required argument <VERSION-ID>\n")
		return "", &exitErr{code: 1, msg: "missing <VERSION-ID>"}
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "error: too many arguments (expected one <VERSION-ID>, got %d)\n", len(positional))
		return "", &exitErr{code: 1, msg: "too many arguments"}
	}
	return positional[0], nil
}

// releaseCommand groups the version-management subcommands under a single
// `release` parent, mirroring how the top-level write commands are wired.
func releaseCommand(env map[string]string) *urfave.Command {
	return &urfave.Command{
		Name:  "release",
		Usage: "Manage Jira project versions (releases)",
		Commands: []*urfave.Command{
			releaseCreateCommand(env),
			releaseUpdateCommand(env),
			releaseListCommand(env),
		},
	}
}

// versionOptionsFromFlags builds the shared optional VersionOptions from
// the flags common to release create/update. Every flag is gated on
// cmd.IsSet so an unset flag is never emitted — on update this means an
// existing value is never blanked, and on create it keeps the dry-run
// body minimal and byte-predictable.
func versionOptionsFromFlags(cmd *urfave.Command) []client.VersionOption {
	var opts []client.VersionOption
	if cmd.IsSet("description") {
		opts = append(opts, client.WithVersionDescription(cmd.String("description")))
	}
	if cmd.IsSet("released") {
		opts = append(opts, client.WithVersionReleased(cmd.Bool("released")))
	}
	if cmd.IsSet("archived") {
		opts = append(opts, client.WithVersionArchived(cmd.Bool("archived")))
	}
	if cmd.IsSet("release-date") {
		opts = append(opts, client.WithVersionReleaseDate(cmd.String("release-date")))
	}
	if cmd.IsSet("start-date") {
		opts = append(opts, client.WithVersionStartDate(cmd.String("start-date")))
	}
	return opts
}

func releaseCreateCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "project", Usage: "Project key (required unless --project-id given)"},
		&urfave.StringFlag{Name: "project-id", Usage: "Numeric project id (takes precedence; enables byte-exact dry-run)"},
		&urfave.StringFlag{Name: "name", Usage: "Version name (required)"},
		&urfave.StringFlag{Name: "description", Usage: "Version description"},
		&urfave.BoolFlag{Name: "released", Usage: "Mark the version released"},
		&urfave.BoolFlag{Name: "archived", Usage: "Mark the version archived"},
		&urfave.StringFlag{Name: "release-date", Usage: "Release date (yyyy-mm-dd)"},
		&urfave.StringFlag{Name: "start-date", Usage: "Start date (yyyy-mm-dd)"},
		&urfave.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be POSTed and exit (no HTTP call)"},
	)
	return &urfave.Command{
		Name:  "create",
		Usage: "Create a new project version (release)",
		Flags: flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runReleaseCreate(ctx, cmd, env)
		},
	}
}

func runReleaseCreate(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	name := cmd.String("name")
	if name == "" {
		fmt.Fprintln(stderr, "error: --name is required")
		return &exitErr{code: 1, msg: "missing --name"}
	}

	projectID := cmd.String("project-id")
	projectKey := cmd.String("project")
	if projectID == "" && projectKey == "" {
		fmt.Fprintln(stderr, "error: one of --project or --project-id is required")
		return &exitErr{code: 1, msg: "missing --project"}
	}

	// --project-id (numeric) takes precedence and yields a byte-exact
	// dry-run; otherwise the key is used and resolved at send time.
	project := projectKey
	usedKeyOnly := true
	if projectID != "" {
		project = projectID
		usedKeyOnly = false
	}

	opts := versionOptionsFromFlags(cmd)

	if cmd.Bool("dry-run") {
		body, err := gojira.BuildCreateVersionBody(name, project, opts...)
		if err != nil {
			fmt.Fprintf(stderr, "error: build create version body: %v\n", err)
			return &exitErr{code: 1, msg: "build create version body", wrap: err}
		}
		fmt.Fprintln(stdout, string(prettyJSON(body)))
		if usedKeyOnly {
			fmt.Fprintln(stderr, "note: --project resolves to projectId at send time; pass --project-id for a byte-exact dry-run")
		}
		return nil
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	v, err := gojira.CreateVersion(ctx, cfg, name, project, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "error: create version: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "create version", wrap: err}
	}
	fmt.Fprintf(stdout, "Created version %s (%s)\n%s\n", v.ID, v.Name, v.Self)
	return nil
}

func releaseUpdateCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "description", Usage: "New description"},
		&urfave.BoolFlag{Name: "released", Usage: "Set released flag"},
		&urfave.BoolFlag{Name: "archived", Usage: "Set archived flag"},
		&urfave.StringFlag{Name: "release-date", Usage: "Release date (yyyy-mm-dd)"},
		&urfave.StringFlag{Name: "start-date", Usage: "Start date (yyyy-mm-dd)"},
		&urfave.BoolFlag{Name: "dry-run", Usage: "Print the JSON body that would be PUT and exit (no HTTP call)"},
	)
	return &urfave.Command{
		Name:      "update",
		Usage:     "Edit an existing project version (release)",
		ArgsUsage: "<VERSION-ID>",
		Flags:     flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runReleaseUpdate(ctx, cmd, env)
		},
	}
}

func runReleaseUpdate(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
	stderr := stderrOf(cmd)
	stdout := stdoutOf(cmd)

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	id, err := requireOneVersionID(cmd, stderr)
	if err != nil {
		return err
	}

	// Only build options for flags the user actually set so unset flags
	// never blank existing values.
	opts := versionOptionsFromFlags(cmd)
	if len(opts) == 0 {
		fmt.Fprintln(stderr, "error: nothing to update — pass at least one of --description/--released/--archived/--release-date/--start-date")
		return &exitErr{code: 1, msg: "nothing to update"}
	}

	if cmd.Bool("dry-run") {
		body, err := gojira.BuildUpdateVersionBody(opts...)
		if err != nil {
			fmt.Fprintf(stderr, "error: build update version body: %v\n", err)
			return &exitErr{code: 1, msg: "build update version body", wrap: err}
		}
		fmt.Fprintln(stdout, string(prettyJSON(body)))
		return nil
	}

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	v, err := gojira.UpdateVersion(ctx, cfg, id, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "error: update version: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "update version", wrap: err}
	}
	fmt.Fprintf(stdout, "Updated version %s\n", v.ID)
	return nil
}

func releaseListCommand(env map[string]string) *urfave.Command {
	conn := connFlags(env)
	flags := append(conn,
		&urfave.StringFlag{Name: "project", Usage: "Project key or id (required)"},
	)
	return &urfave.Command{
		Name:  "list",
		Usage: "List the versions (releases) for a project",
		Flags: flags,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runReleaseList(ctx, cmd, env)
		},
	}
}

func runReleaseList(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
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

	cfg, err := loadWriteConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	vs, err := gojira.ListVersions(ctx, cfg, project)
	if err != nil {
		fmt.Fprintf(stderr, "error: list versions: %v\n", err)
		printAPIError(stderr, err)
		return &exitErr{code: 1, msg: "list versions", wrap: err}
	}
	if len(vs) == 0 {
		fmt.Fprintf(stdout, "No versions for %s\n", project)
		return nil
	}
	for _, v := range vs {
		fmt.Fprintf(stdout, "%s\t%s\treleased=%t\t%s\n", v.ID, v.Name, v.Released, v.ReleaseDate)
	}
	return nil
}
