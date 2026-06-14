// Package cli implements the gojira command-line interface (subcommand
// wiring, flag definitions, the config cascade glue, the run
// orchestrator, signal handling, and exit-code mapping). It is the
// internal home for everything cmd/gojira used to carry as package
// main; the binary is now a thin entrypoint that calls [Run].
//
// The CLI is never imported by third parties, hence the placement
// under internal/. The package's urfave/cli/v3 import is aliased to
// `urfave` to avoid colliding with the package name.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/pkg/log"
	urfave "github.com/urfave/cli/v3"
)

// versionPrinterOnce guards the one-time install of the package-level
// urfave.VersionPrinter override. We override it so --version emits the
// historical "gojira v0.1.0" format rather than cli's default
// "gojira version v0.1.0" — preserving the externally-observable
// behavior committed in PRD §13.
func init() {
	urfave.VersionPrinter = func(cmd *urfave.Command) {
		_, _ = fmt.Fprintf(cmd.Root().Writer, "%s %s\n", cmd.Name, cmd.Version)
	}
}

// Run is the exported entry point the cmd/gojira binary calls. It
// delegates to the unexported run orchestrator so existing white-box
// tests (which call run directly) keep working unchanged. Run is the
// only signature the binary depends on; expanding it to a struct in a
// future phase is safe.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, env map[string]string) int {
	return run(ctx, args, stdout, stderr, env)
}

// EnvMap is the exported wrapper around envMap. It reads all GOJIRA_*
// environment variables once at process start and returns them as a
// map[string]string so the binary can hand them to Run.
func EnvMap() map[string]string {
	return envMap()
}

// envMap reads all GOJIRA_* environment variables and returns them as a
// map[string]string. This is the only place os.Getenv is called; the rest of
// the program works with the map so tests can inject arbitrary values.
func envMap() map[string]string {
	m := make(map[string]string)
	for _, key := range allEnvKeys() {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	return m
}

// allEnvKeys returns every GOJIRA_* env key the CLI consults. The union
// covers the legacy v0.1 flat keys (the Sources chain of each crawl flag
// reads them directly so the existing CLI behavior is preserved), the
// canonical Phase 0 keys (so the LoadAppConfig cascade sees them when the
// user has migrated their environment), and the new GOJIRA_CONFIG_FILE
// override. Deprecated aliases are sourced from
// [config.DeprecatedAliasKeys] so the table stays in sync if a new alias
// is added later.
func allEnvKeys() []string {
	// Canonical Phase 0 + GOJIRA_CONFIG_FILE.
	canonical := []string{
		"GOJIRA_CONFIG_FILE",
		"GOJIRA_OUTPUT_DIR",
		"GOJIRA_LOG_LEVEL",
		"GOJIRA_LOG_FORMAT",
		"GOJIRA_JIRA_BASE_URL",
		"GOJIRA_JIRA_EMAIL",
		"GOJIRA_JIRA_API_TOKEN",
		"GOJIRA_CRAWL_DEPTH_LIMIT",
		"GOJIRA_CRAWL_ISSUE_CAP",
		"GOJIRA_CRAWL_TIME_CAP_SECONDS",
		"GOJIRA_CRAWL_CONCURRENCY",
		"GOJIRA_CRAWL_REFETCH",
		"GOJIRA_CRAWL_INCLUDE_COMMENTS",
		"GOJIRA_CRAWL_INCLUDE_CHILDREN",
		"GOJIRA_CRAWL_CHILD_SEARCH_LIMIT",
		"GOJIRA_CRAWL_EPIC_LINK_FIELD",
		"GOJIRA_CRAWL_INCLUDE_DEV_STATUS",
		"GOJIRA_CRAWL_DEV_STATUS_APPLICATIONS",
		"GOJIRA_CRAWL_DEV_STATUS_DATA_TYPES",
		"GOJIRA_CRAWL_RENDER_NULL_CUSTOM_FIELDS",
		"GOJIRA_CRAWL_EMIT_GRAPH",
		"GOJIRA_MCP_MODE",
		"GOJIRA_MCP_ALLOW_WRITES",
		"GOJIRA_SERVER_ADDRESS",
	}
	// Deprecated v0.1 flat aliases — sourced from the config package
	// so the table is the single source of truth.
	aliases := config.DeprecatedAliasKeys()

	// Deduplicate (canonical and alias sets are disjoint, but be
	// defensive against a future overlap).
	seen := make(map[string]struct{}, len(canonical)+len(aliases))
	out := make([]string, 0, len(canonical)+len(aliases))
	for _, k := range canonical {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	for _, k := range aliases {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// Exit-code sentinel error
// ---------------------------------------------------------------------------

// exitErr lets the Action return an error that encodes a specific exit
// code. run() inspects this via errors.As to decide the final exit code.
// Any non-exitErr error from cmd.Run maps to exit 1.
type exitErr struct {
	code int
	msg  string
	wrap error
}

func (e *exitErr) Error() string {
	if e.wrap != nil {
		if e.msg != "" {
			return e.msg + ": " + e.wrap.Error()
		}
		return e.wrap.Error()
	}
	return e.msg
}

func (e *exitErr) Unwrap() error { return e.wrap }

// ---------------------------------------------------------------------------
// urfave.ValueSource backed by the injected env map
// ---------------------------------------------------------------------------

// mapValueSource is a urfave.ValueSource that reads a single key out of an
// env map[string]string captured by closure at run() time. This is the
// testability seam: tests inject the env map directly, and production
// code populates the map from os.Getenv in envMap(). The CLI library
// only sees this source, so the rest of the urfave/cli machinery works
// uniformly in both modes.
type mapValueSource struct {
	key string
	env map[string]string
}

func newMapValueSource(env map[string]string, key string) urfave.ValueSource {
	return &mapValueSource{key: key, env: env}
}

func (m *mapValueSource) Lookup() (string, bool) {
	if m.env == nil {
		return "", false
	}
	v, ok := m.env[m.key]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

func (m *mapValueSource) String() string {
	return fmt.Sprintf("env map %q", m.key)
}

func (m *mapValueSource) GoString() string {
	return fmt.Sprintf("&mapValueSource{key:%q}", m.key)
}

// ---------------------------------------------------------------------------
// run — the testable entry point
// ---------------------------------------------------------------------------

// run is the testable entry point. It returns the exit code.
//
// args is os.Args (args[0] is the program name).
// stdout receives --help and --version output only.
// stderr receives all progress events, the summary, and error messages.
// env is the GOJIRA_* environment variable map (flags override these).
func run(ctx context.Context, args []string, stdout, stderr io.Writer, env map[string]string) int {
	// PRD §9 / iron rule 7: missing subcommand → exit 1 with usage on
	// stderr. We handle this before delegating to cli so the exit code
	// is unambiguous (cli's default for a bare invocation would print
	// help and exit 0).
	if len(args) < 2 {
		printShortUsage(stderr)
		return 1
	}

	// Track whether a signal was received; used both by the action and
	// by the post-cmd.Run exit-code mapping.
	var signalled atomic.Bool

	// Track unknown-subcommand: cli's CommandNotFound callback is fire-
	// and-forget — it cannot return an error. We capture the event in a
	// local bool so the exit-code mapping below can promote it to exit 1.
	var unknownSubcommand atomic.Bool

	// Crawl context derives from ctx so external cancellation (test
	// timeouts) still works, but signal handlers add a graceful path on
	// top of it.
	crawlCtx, cancelCrawl := context.WithCancel(ctx)
	defer cancelCrawl()

	// Install signal handlers. First signal cancels the context (graceful
	// shutdown). Second signal hard-exits with code 130 (POSIX SIGINT
	// convention).
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if signalled.Swap(true) {
				fmt.Fprintf(stderr, "\ngojira: second signal (%s) received — force-quitting\n", sig)
				os.Exit(130)
			}
			fmt.Fprintf(stderr, "\ngojira: signal (%s) received — shutting down gracefully (send again to force-quit)\n", sig)
			cancelCrawl()
		}
	}()
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
	}()

	// Build the root command.
	cmd := buildRootCommand(env, &signalled, &unknownSubcommand, stdout, stderr)
	cmd.Writer = stdout
	cmd.ErrWriter = stderr

	// Run. cmd.Run returns whatever the action returned (after cli's
	// ExitErrHandler, which we set to a no-op so we own error printing).
	err := cmd.Run(crawlCtx, args)

	// --- exit-code mapping ---

	var ee *exitErr
	if errors.As(err, &ee) {
		return ee.code
	}

	if err == nil {
		// CommandNotFound fires without returning an error; promote it
		// here so unknown subcommands map to exit 1.
		if unknownSubcommand.Load() {
			return 1
		}
		// Action returned nil. Check for signal interruption that
		// happened to race with a successful crawl completion.
		if signalled.Load() {
			return 2
		}
		// External context cancellation (e.g. test timeout) with no
		// reported error: treat as interrupted.
		if ctx.Err() != nil {
			return 2
		}
		return 0
	}

	// Any other error from cli (flag parse, unknown subcommand handled
	// via CommandNotFound, etc.) → exit 1. cli has already printed the
	// underlying message to ErrWriter.
	return 1
}

// ---------------------------------------------------------------------------
// Root command construction
// ---------------------------------------------------------------------------

// buildRootCommand constructs the *urfave.Command tree. env, signalled,
// unknownSubcommand, and the writer pair are captured by closure so the
// action has everything it needs without further wiring.
func buildRootCommand(env map[string]string, signalled *atomic.Bool, unknownSubcommand *atomic.Bool, stdout, stderr io.Writer) *urfave.Command {
	return &urfave.Command{
		Name:    "gojira",
		Usage:   "Jira-to-Markdown mirror tool",
		Version: gojira.Version,
		// EnableShellCompletion injects an auto-generated
		// `gojira completion <bash|zsh|fish|pwsh>` subcommand
		// (urfave/cli/v3 ≥ v3.0). The completion logic is derived
		// from the command tree, so adding a new subcommand or flag
		// automatically extends completion without further wiring.
		// The completion command does NOT call requireConfig (the
		// guard runs inside each Jira-touching command's Action,
		// never globally), so completion works on a fresh box with
		// no config — same UX as `init`, `--help`, and `--version`.
		EnableShellCompletion: true,
		// ConfigureShellCompletionCommand pins the auto-injected
		// completion subcommand's Writer/ErrWriter to the SAME
		// stdout/stderr the rest of the CLI uses. Without this,
		// urfave/cli/v3 v3.9.0's setupDefaults sets the subcommand's
		// Writer to os.Stdout directly when it is nil — bypassing
		// any cmd.Writer assignment made on the parent (cf.
		// command_setup.go:53 and completion.go:96). The override
		// keeps test capture working AND keeps `gojira completion
		// <shell>` consistent with `gojira init`, `gojira crawl`,
		// etc. when callers redirect stdout/stderr explicitly.
		ConfigureShellCompletionCommand: func(c *urfave.Command) {
			c.Writer = stdout
			c.ErrWriter = stderr
		},
		// Description appears in --help between the NAME line and the
		// flags/commands block. The opening paragraph surveys the
		// whole command surface so help readers see more than crawl
		// at a glance; the crawl-specific paragraph + the exit-code
		// legend below it remain because crawl is still the headline
		// command and the codes accurately describe its behaviour.
		Description: `gojira mirrors Jira Cloud into Markdown and exposes the same engine over
a gRPC server (serve), an MCP server (mcp), and issue write operations.
Run 'gojira <command> --help' for command-specific flags.

The crawl subcommand fetches a Jira issue and all issues reachable from
it, writing Markdown files to the configured output directory.

Exit codes:
  0   All issues fetched successfully.
  1   Total failure (auth error, config error, nothing rendered).
  2   Partial success or signal-interrupted crawl.
  130 Force-quit by second signal.`,
		Commands: []*urfave.Command{
			crawlCommand(env, signalled),
			serveCommand(env, signalled),
			createCommand(env),
			updateCommand(env),
			commentCommand(env),
			transitionsCommand(env),
			transitionCommand(env),
			initCommand(env),
			mcpCommand(env, signalled),
		},
		// We map errors to exit codes ourselves in run(); cli's default
		// would call HandleExitCoder which may os.Exit. Suppress it.
		ExitErrHandler: func(ctx context.Context, cmd *urfave.Command, err error) {
			// no-op: error is returned from cmd.Run and handled in run()
		},
		CommandNotFound: func(ctx context.Context, cmd *urfave.Command, name string) {
			unknownSubcommand.Store(true)
			fmt.Fprintf(stderr, "error: unknown subcommand %q\n", name)
		},
		// Suggest=false: stay close to the legacy hand-rolled behavior.
		Suggest: false,
	}
}

// crawlCommand returns the *urfave.Command for "gojira crawl".
func crawlCommand(env map[string]string, signalled *atomic.Bool) *urfave.Command {
	return &urfave.Command{
		Name:      "crawl",
		Usage:     "Fetch a Jira issue and recursively mirror its graph to Markdown",
		ArgsUsage: "<ISSUE-KEY>",
		Flags:     crawlFlags(env),
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			return runCrawl(ctx, cmd, env, signalled)
		},
	}
}

// crawlFlags declares every flag the crawl subcommand accepts. Each flag's
// Sources chain reads from the injected env map; precedence is therefore
// command-line flag (highest) → env map (which production code derives
// from os.Getenv).
func crawlFlags(env map[string]string) []urfave.Flag {
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
		&urfave.StringFlag{
			Name:    "output-dir",
			Usage:   "Output root directory",
			Sources: src("GOJIRA_OUTPUT_DIR"),
		},
		&urfave.IntFlag{
			Name:    "depth-limit",
			Usage:   "Max crawl depth from start issue (0 = unlimited)",
			Sources: src("GOJIRA_DEPTH_LIMIT"),
		},
		&urfave.IntFlag{
			Name:    "issue-cap",
			Usage:   "Max issues to fetch per run (0 = use default 500)",
			Sources: src("GOJIRA_ISSUE_CAP"),
		},
		&urfave.IntFlag{
			Name:    "time-cap",
			Usage:   "Max wall-clock seconds per run (0 = unlimited)",
			Sources: src("GOJIRA_TIME_CAP_SECONDS"),
		},
		&urfave.IntFlag{
			Name:    "concurrency",
			Usage:   "Concurrent Jira API requests (0 = use default 3)",
			Sources: src("GOJIRA_CONCURRENCY"),
		},
		&urfave.BoolFlag{
			Name:    "refetch",
			Usage:   "Re-fetch issues already on disk",
			Sources: src("GOJIRA_REFETCH"),
		},
		&urfave.BoolFlag{
			Name:    "include-comments",
			Usage:   "Fetch and render issue comments",
			Sources: src("GOJIRA_INCLUDE_COMMENTS"),
		},
		&urfave.StringFlag{
			Name:    "log-level",
			Usage:   "Log verbosity: error|warn|info|debug|trace",
			Sources: src("GOJIRA_LOG_LEVEL"),
		},
		&urfave.StringFlag{
			Name:    "log-format",
			Usage:   "Log output format: text|json",
			Sources: src("GOJIRA_LOG_FORMAT"),
		},
		&urfave.BoolFlag{
			Name:    "include-children",
			Usage:   "Discover hierarchy children via JQL search after each fetch (default true)",
			Sources: src("GOJIRA_INCLUDE_CHILDREN"),
		},
		&urfave.IntFlag{
			Name:    "child-search-limit",
			Usage:   "Max hierarchy children to discover per parent issue (default 100)",
			Sources: src("GOJIRA_CHILD_SEARCH_LIMIT"),
		},
		&urfave.StringFlag{
			Name:    "epic-link-field",
			Usage:   "Optional Epic Link custom field ID (e.g. customfield_10014); auto-detected when empty",
			Sources: src("GOJIRA_EPIC_LINK_FIELD"),
		},
		&urfave.BoolFlag{
			Name:    "include-dev-status",
			Usage:   "Query Jira Dev Status API for pull-request URLs per issue (default true)",
			Sources: src("GOJIRA_INCLUDE_DEV_STATUS"),
		},
		&urfave.StringFlag{
			Name:    "dev-status-applications",
			Usage:   "Comma-separated Dev Status applicationType values to query (default GitHub)",
			Sources: src("GOJIRA_DEV_STATUS_APPLICATIONS"),
		},
		&urfave.StringFlag{
			Name:    "dev-status-data-types",
			Usage:   "Comma-separated Dev Status dataType values to query (default pullrequest,branch,commit,repository,build)",
			Sources: src("GOJIRA_DEV_STATUS_DATA_TYPES"),
		},
		&urfave.BoolFlag{
			Name:    "render-null-custom-fields",
			Usage:   "Render custom fields whose value is JSON null (default false skips them)",
			Sources: src("GOJIRA_RENDER_NULL_CUSTOM_FIELDS"),
		},
		&urfave.BoolFlag{
			Name:    "graph",
			Usage:   "Write graph.json and graph.d2 (D2 source) at the output-dir root",
			Sources: src("GOJIRA_EMIT_GRAPH"),
		},
	}
}

// ---------------------------------------------------------------------------
// crawl action
// ---------------------------------------------------------------------------

// runCrawl is the body of the "gojira crawl" subcommand. It validates the
// positional argument, builds the config kv map (collapsing flag + env
// into a single GOJIRA_* dictionary), constructs the logger and sink,
// invokes gojira.Crawl, prints the summary, and returns an *exitErr
// whose code drives run()'s final exit code.
func runCrawl(ctx context.Context, cmd *urfave.Command, env map[string]string, signalled *atomic.Bool) error {
	stderr := cmd.Root().ErrWriter
	if stderr == nil {
		stderr = os.Stderr
	}

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	// Exactly one positional argument: <ISSUE-KEY>.
	positional := cmd.Args().Slice()
	if len(positional) == 0 {
		fmt.Fprintf(stderr, "error: missing required argument <ISSUE-KEY>\n")
		return &exitErr{code: 1, msg: "missing <ISSUE-KEY>"}
	}
	if len(positional) > 1 {
		fmt.Fprintf(stderr, "error: too many arguments (expected one <ISSUE-KEY>, got %d)\n", len(positional))
		return &exitErr{code: 1, msg: "too many arguments"}
	}
	issueKey := positional[0]

	// Phase 5 cascade. Configuration is built in three steps so the
	// documented precedence (file < env < flag) is preserved while
	// validation continues to flow through the single canonical
	// LoadConfig pass — guaranteeing the legacy *ConfigError surface
	// and the existing user-facing error messages (e.g. "GOJIRA_SITE
	// is required") are unchanged.
	//
	//  1) Run the app-level cascade (embedded defaults < YAML file)
	//     via the loader package. Env parsing and the Layer-2
	//     validator are NOT run here — env is handled below in the
	//     legacy LoadConfig pass, and validation belongs there too
	//     so error messages match the v0.1 surface that downstream
	//     tests and users depend on.
	//
	//  2) Flatten the file-layer Config to a kv map, then overlay
	//     the env layer and the flag-or-env-source layer in
	//     precedence order: alias-resolved env values land first,
	//     then user-typed CLI flags (cli's hasBeenSet semantics
	//     ensure flag values dominate env-source values at the
	//     buildConfigKV layer).
	//
	//  3) Run gojira.LoadConfig on the merged kv map. This is the
	//     single validation pass: URL parseability, enums, integers,
	//     and required-field errors all surface here through the
	//     existing *ConfigError surface.
	configPath := cmd.String("config")
	fileCfg, err := gojira.LoadFileConfig(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return &exitErr{code: 1, msg: "config", wrap: err}
	}

	mergedKV := configToKV(fileCfg)
	// Env layer (alias-resolved so legacy GOJIRA_SITE-style keys
	// continue to populate the canonical Phase 0 names in the
	// merged map).
	for k, v := range config.ResolveAliases(env) {
		if v != "" {
			mergedKV[k] = v
		}
	}
	// Flag-or-env-source layer (urfave/cli's IsSet returns true for
	// either input). User-typed flags win over env-source values at
	// this layer due to cli's hasBeenSet semantics.
	for k, v := range buildConfigKV(cmd) {
		mergedKV[k] = v
	}

	cfg, err := gojira.LoadConfig(mergedKV)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return &exitErr{code: 1, msg: "config", wrap: err}
	}

	// Construct the slog-backed events sink.
	//
	// cfg.LogFormat has already been validated by config.Build to be
	// one of "text" or "json", so ParseFormat cannot return a real
	// error here; we still discard it explicitly for the same reason.
	format, _ := log.ParseFormat(cfg.LogFormat)

	// cfg.LogLevel has likewise been validated against {error, warn,
	// info, debug, trace}. log.ParseLevel accepts "trace" (slog's
	// UnmarshalText does NOT — it only knows the four built-in
	// levels), and matches the validator's accepted set exactly.
	// Validation has already run upstream, so the only way ParseLevel
	// fails here is a code-level inconsistency between the validator
	// and the parser — discard the error to keep the existing
	// no-error surface, mirroring the previous UnmarshalText pattern.
	slevel, _ := log.ParseLevel(cfg.LogLevel)

	logger := log.New(format, slevel, stderr)
	sink := gojira.NewSlogSink(logger)

	// Run the crawl.
	start := time.Now()
	summary, crawlErr := gojira.CrawlWithLogger(ctx, cfg, []string{issueKey}, sink, logger)
	elapsed := time.Since(start)

	// Print the summary report to stderr (PRD AC 18, unchanged format).
	printSummary(stderr, summary, elapsed)

	// Map crawl outcome to an exit code via *exitErr.
	return mapCrawlOutcome(stderr, summary, crawlErr, ctx, signalled)
}

// configToKV flattens a [gojira.Config] back to the canonical legacy
// GOJIRA_* kv map [gojira.LoadConfig] expects. It is the inverse of
// [gojira.LoadConfig] for the fields used by the CLI. The CLI uses it
// to feed the file+env result of the Phase 5 cascade into LoadConfig's
// single validation pass after overlaying any user-typed flag values.
// Keeping the conversion centralized here avoids drift between the new
// cascade's field names and the legacy GOJIRA_* keys downstream of the
// envext parser.
func configToKV(cfg gojira.Config) map[string]string {
	bool01 := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	return map[string]string{
		"GOJIRA_SITE":                      cfg.Site,
		"GOJIRA_USER":                      cfg.User,
		"GOJIRA_TOKEN":                     cfg.Token,
		"GOJIRA_OUTPUT_DIR":                cfg.OutputDir,
		"GOJIRA_DEPTH_LIMIT":               fmt.Sprintf("%d", cfg.DepthLimit),
		"GOJIRA_ISSUE_CAP":                 fmt.Sprintf("%d", cfg.IssueCap),
		"GOJIRA_TIME_CAP_SECONDS":          fmt.Sprintf("%d", cfg.TimeCapSeconds),
		"GOJIRA_CONCURRENCY":               fmt.Sprintf("%d", cfg.Concurrency),
		"GOJIRA_REFETCH":                   bool01(cfg.Refetch),
		"GOJIRA_INCLUDE_COMMENTS":          bool01(cfg.IncludeComments),
		"GOJIRA_LOG_LEVEL":                 cfg.LogLevel,
		"GOJIRA_LOG_FORMAT":                cfg.LogFormat,
		"GOJIRA_INCLUDE_CHILDREN":          bool01(cfg.IncludeChildren),
		"GOJIRA_CHILD_SEARCH_LIMIT":        fmt.Sprintf("%d", cfg.ChildSearchLimit),
		"GOJIRA_EPIC_LINK_FIELD":           cfg.EpicLinkField,
		"GOJIRA_INCLUDE_DEV_STATUS":        bool01(cfg.IncludeDevStatus),
		"GOJIRA_DEV_STATUS_APPLICATIONS":   strings.Join(cfg.DevStatusApplications, ","),
		"GOJIRA_DEV_STATUS_DATA_TYPES":     strings.Join(cfg.DevStatusDataTypes, ","),
		"GOJIRA_RENDER_NULL_CUSTOM_FIELDS": bool01(cfg.RenderNullCustomFields),
		"GOJIRA_EMIT_GRAPH":                bool01(cfg.EmitGraph),
		"GOJIRA_MCP_MODE":                  cfg.MCPMode,
		"GOJIRA_MCP_ALLOW_WRITES":          bool01(cfg.MCPAllowWrites),
		"GOJIRA_SERVER_ADDRESS":            cfg.ServerAddress,
	}
}

// buildConfigKV collapses cmd's per-flag state into the canonical
// GOJIRA_* key/value map expected by gojira.LoadConfig.
//
// A flag is considered "present" if urfave.Command.IsSet reports it set —
// which is true when the user passed it on the command line OR when one
// of its Sources (i.e. our injected env-map source) resolved a value.
// The two cases together replicate the previous behavior of "flag wins
// over env, otherwise env wins" without an explicit overlay step.
func buildConfigKV(cmd *urfave.Command) map[string]string {
	kv := make(map[string]string, 12)

	if cmd.IsSet("site") {
		kv["GOJIRA_SITE"] = cmd.String("site")
	}
	if cmd.IsSet("user") {
		kv["GOJIRA_USER"] = cmd.String("user")
	}
	if cmd.IsSet("token") {
		kv["GOJIRA_TOKEN"] = cmd.String("token")
	}
	if cmd.IsSet("output-dir") {
		kv["GOJIRA_OUTPUT_DIR"] = cmd.String("output-dir")
	}
	if cmd.IsSet("depth-limit") {
		kv["GOJIRA_DEPTH_LIMIT"] = fmt.Sprintf("%d", cmd.Int("depth-limit"))
	}
	if cmd.IsSet("issue-cap") {
		kv["GOJIRA_ISSUE_CAP"] = fmt.Sprintf("%d", cmd.Int("issue-cap"))
	}
	if cmd.IsSet("time-cap") {
		kv["GOJIRA_TIME_CAP_SECONDS"] = fmt.Sprintf("%d", cmd.Int("time-cap"))
	}
	if cmd.IsSet("concurrency") {
		kv["GOJIRA_CONCURRENCY"] = fmt.Sprintf("%d", cmd.Int("concurrency"))
	}
	if cmd.IsSet("refetch") {
		if cmd.Bool("refetch") {
			kv["GOJIRA_REFETCH"] = "true"
		} else {
			kv["GOJIRA_REFETCH"] = "false"
		}
	}
	if cmd.IsSet("include-comments") {
		if cmd.Bool("include-comments") {
			kv["GOJIRA_INCLUDE_COMMENTS"] = "true"
		} else {
			kv["GOJIRA_INCLUDE_COMMENTS"] = "false"
		}
	}
	if cmd.IsSet("log-level") {
		kv["GOJIRA_LOG_LEVEL"] = cmd.String("log-level")
	}
	if cmd.IsSet("log-format") {
		kv["GOJIRA_LOG_FORMAT"] = cmd.String("log-format")
	}
	if cmd.IsSet("include-children") {
		if cmd.Bool("include-children") {
			kv["GOJIRA_INCLUDE_CHILDREN"] = "true"
		} else {
			kv["GOJIRA_INCLUDE_CHILDREN"] = "false"
		}
	}
	if cmd.IsSet("child-search-limit") {
		kv["GOJIRA_CHILD_SEARCH_LIMIT"] = fmt.Sprintf("%d", cmd.Int("child-search-limit"))
	}
	if cmd.IsSet("epic-link-field") {
		kv["GOJIRA_EPIC_LINK_FIELD"] = cmd.String("epic-link-field")
	}
	if cmd.IsSet("include-dev-status") {
		if cmd.Bool("include-dev-status") {
			kv["GOJIRA_INCLUDE_DEV_STATUS"] = "true"
		} else {
			kv["GOJIRA_INCLUDE_DEV_STATUS"] = "false"
		}
	}
	if cmd.IsSet("dev-status-applications") {
		kv["GOJIRA_DEV_STATUS_APPLICATIONS"] = cmd.String("dev-status-applications")
	}
	if cmd.IsSet("dev-status-data-types") {
		kv["GOJIRA_DEV_STATUS_DATA_TYPES"] = cmd.String("dev-status-data-types")
	}
	if cmd.IsSet("render-null-custom-fields") {
		if cmd.Bool("render-null-custom-fields") {
			kv["GOJIRA_RENDER_NULL_CUSTOM_FIELDS"] = "true"
		} else {
			kv["GOJIRA_RENDER_NULL_CUSTOM_FIELDS"] = "false"
		}
	}
	if cmd.IsSet("graph") {
		if cmd.Bool("graph") {
			kv["GOJIRA_EMIT_GRAPH"] = "true"
		} else {
			kv["GOJIRA_EMIT_GRAPH"] = "false"
		}
	}
	// --address is declared only by the serve and mcp subcommands;
	// urfave/cli/v3 IsSet returns false for any unknown flag, so this
	// is safe to call from crawl too.
	if cmd.IsSet("address") {
		kv["GOJIRA_SERVER_ADDRESS"] = cmd.String("address")
	}

	return kv
}

// mapCrawlOutcome translates a (summary, crawlErr) pair into an
// *exitErr whose code follows PRD §9.
//
// Exit code mapping:
//
//	1  — auth failure (ErrUnauthorized).
//	1  — other fatal error with nothing rendered.
//	2  — context cancelled by signal (graceful shutdown).
//	2  — partial success: some rendered, some failed or cap-limited.
//	0  — full success: no failures, no stubs, no cap-limits.
//
// Note: stubbed issues (403/404) are NOT counted as failures for exit
// purposes because a stub file IS written.
func mapCrawlOutcome(stderr io.Writer, summary gojira.Summary, crawlErr error, ctx context.Context, signalled *atomic.Bool) error {
	if crawlErr != nil {
		if errors.Is(crawlErr, gojira.ErrUnauthorized) {
			fmt.Fprintf(stderr, "error: authentication failed (401) — check GOJIRA_USER and GOJIRA_TOKEN\n")
			return &exitErr{code: 1, msg: "unauthorized", wrap: crawlErr}
		}
		// Signal-initiated cancellation → partial/interrupted.
		if errors.Is(crawlErr, context.Canceled) || signalled.Load() {
			return &exitErr{code: 2, msg: "interrupted", wrap: crawlErr}
		}
		// Other fatal error with nothing rendered.
		if summary.Fetched+summary.Stubbed == 0 {
			fmt.Fprintf(stderr, "error: crawl failed: %v\n", crawlErr)
			return &exitErr{code: 1, msg: "crawl failed", wrap: crawlErr}
		}
		// Some rendered despite the error → partial.
		return &exitErr{code: 2, msg: "partial", wrap: crawlErr}
	}

	// No error from crawl.
	if signalled.Load() {
		return &exitErr{code: 2, msg: "interrupted (signal)"}
	}
	if ctx.Err() != nil {
		// Parent ctx was cancelled (e.g. test timeout / external deadline).
		return &exitErr{code: 2, msg: "interrupted (ctx)"}
	}
	if summary.Failed == 0 && summary.CapLimited == 0 {
		return nil
	}
	if summary.Fetched+summary.Stubbed > 0 {
		return &exitErr{code: 2, msg: "partial success"}
	}
	if summary.CapLimited > 0 {
		return &exitErr{code: 2, msg: "cap-limited"}
	}
	return &exitErr{code: 1, msg: "nothing rendered"}
}

// ---------------------------------------------------------------------------
// Short usage (early-exit when no subcommand is given)
// ---------------------------------------------------------------------------

// printShortUsage writes a compact usage block to w. This is used when
// the binary is invoked with no arguments at all — the case where we
// cannot hand off to urfave.Run without it deciding to print help and
// exit 0. The block enumerates the full command surface so users
// discover non-crawl subcommands (init, serve, mcp, …) without having
// to read --help first. Command descriptions here mirror each
// command's own Usage field; do not invent capabilities.
func printShortUsage(w io.Writer) {
	fmt.Fprintf(w, `gojira %s — Jira-to-Markdown mirror tool

Usage:
  gojira <command> [flags] [args]

Commands:
  crawl         Fetch a Jira issue and recursively mirror its graph to Markdown
  serve         Run the gojira gRPC server
  mcp           Run the gojira MCP server over stdio
  init          Create a gojira config file (--local for a project-local ./gojira.yaml)
  create        Create a new Jira issue
  update        Edit fields on an existing Jira issue
  comment       Add a comment to a Jira issue
  transitions   List the workflow transitions currently available for an issue
  transition    Move an issue through a workflow transition

Run 'gojira <command> --help' for command-specific flags.
Run 'gojira completion <bash|zsh|fish|pwsh>' to generate shell completions.
  gojira --help
  gojira --version
`, gojira.Version)
}

// ---------------------------------------------------------------------------
// Summary printer (unchanged plain-text format — PRD AC 18)
// ---------------------------------------------------------------------------

// printSummary writes the crawl summary report to w.
func printSummary(w io.Writer, s gojira.Summary, elapsed time.Duration) {
	fmt.Fprintln(w, "=== gojira crawl summary ===")
	fmt.Fprintf(w, "fetched:     %d\n", s.Fetched)
	fmt.Fprintf(w, "skipped:     %d\n", s.Skipped)

	if len(s.StubbedKeys) > 0 {
		fmt.Fprintf(w, "stubbed:     %d (keys: %s)\n", s.Stubbed, strings.Join(s.StubbedKeys, ", "))
	} else {
		fmt.Fprintf(w, "stubbed:     %d\n", s.Stubbed)
	}

	if len(s.FailedKeys) > 0 {
		parts := make([]string, 0, len(s.FailedKeys))
		keys := make([]string, 0, len(s.FailedKeys))
		for k := range s.FailedKeys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, k+":"+s.FailedKeys[k])
		}
		fmt.Fprintf(w, "failed:      %d (keys: %s)\n", s.Failed, strings.Join(parts, ", "))
	} else {
		fmt.Fprintf(w, "failed:      %d\n", s.Failed)
	}

	if len(s.CapLimitedKeys) > 0 {
		fmt.Fprintf(w, "cap-limited: %d (keys: %s)\n", s.CapLimited, strings.Join(s.CapLimitedKeys, ", "))
	} else {
		fmt.Fprintf(w, "cap-limited: %d\n", s.CapLimited)
	}

	fmt.Fprintf(w, "pr-refs:     %d\n", s.PRsFound)
	fmt.Fprintf(w, "duration:    %.3f s\n", elapsed.Seconds())
	fmt.Fprintln(w, "============================")
}
