package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	cli "github.com/urfave/cli/v3"

	gojira "github.com/neumachen/gojira"
	"github.com/neumachen/gojira/internal/config"
	gojiragrpc "github.com/neumachen/gojira/internal/grpc"
)

// ---------------------------------------------------------------------------
// serve subcommand
// ---------------------------------------------------------------------------

// serveCommand returns the *cli.Command for "gojira serve". The serve
// command boots a gRPC server that exposes the same library facade the
// CLI uses (Classify, GetIssue, Crawl), letting other processes drive
// gojira without spawning a child binary.
func serveCommand(env map[string]string, signalled *atomic.Bool) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Run the gojira gRPC server",
		Flags: serveFlags(env),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runServe(ctx, cmd, env, signalled)
		},
	}
}

// serveFlags declares the flags the serve subcommand accepts. The set is
// intentionally smaller than [crawlFlags]: serve only needs the
// credentials and output settings the internal/grpc handlers actually
// reach for, plus the server bind address and the logging knobs. Crawl-
// specific knobs (caps, dev-status filters, etc.) come from the file +
// env layers of the configuration cascade and are not flag-overridable
// from the serve command.
func serveFlags(env map[string]string) []cli.Flag {
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
		&cli.StringFlag{
			Name:    "output-dir",
			Usage:   "Output root directory",
			Sources: src("GOJIRA_OUTPUT_DIR"),
		},
		&cli.StringFlag{
			Name:    "address",
			Usage:   "gRPC server bind address (default 127.0.0.1:50051)",
			Sources: src("GOJIRA_SERVER_ADDRESS"),
		},
		&cli.StringFlag{
			Name:    "log-level",
			Usage:   "Log verbosity: error|warn|info|debug",
			Sources: src("GOJIRA_LOG_LEVEL"),
		},
		&cli.StringFlag{
			Name:    "log-format",
			Usage:   "Log output format: text|json",
			Sources: src("GOJIRA_LOG_FORMAT"),
		},
	}
}

// runServe is the body of the "gojira serve" subcommand.
//
// It applies the require-config guard, runs the same file < env < flag
// cascade that runCrawl uses (driven through [gojira.LoadConfig] so the
// user-facing error surface stays identical), then hands the validated
// Config to [gojiragrpc.Serve], which owns the listener, grpc.Server,
// service registration, and graceful shutdown.
//
// Shutdown is driven by ctx, which run() wires to SIGINT/SIGTERM via
// the same signalled atomic the crawl command uses. A clean
// signal-driven shutdown returns nil (exit code 0): a long-running
// server that exits cleanly when asked to is the expected lifecycle,
// not a partial success. Only a non-nil error from
// [gojiragrpc.Serve] produces a non-zero exit.
func runServe(ctx context.Context, cmd *cli.Command, env map[string]string, signalled *atomic.Bool) error {
	_ = signalled // mirrored from runCrawl for API consistency; Serve treats ctx cancel as clean shutdown
	stderr := cmd.Root().ErrWriter
	if stderr == nil {
		stderr = os.Stderr
	}

	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	cfg, err := loadServeConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	if err := gojiragrpc.Serve(ctx, cfg); err != nil {
		fmt.Fprintf(stderr, "error: serve: %v\n", err)
		return &exitErr{code: 1, msg: "serve", wrap: err}
	}
	return nil
}

// loadServeConfig runs the same file < env < flag cascade that runCrawl
// uses, but driven by serve's flag set. Errors are printed to stderr in
// the same format runCrawl uses so the two subcommands present a
// uniform failure surface.
func loadServeConfig(cmd *cli.Command, env map[string]string, stderr io.Writer) (gojira.Config, error) {
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
	// buildConfigKV checks cmd.IsSet for every flag the crawl command
	// declares. urfave/cli/v3's IsSet returns false for any flag the
	// command did not declare (no panic), so reusing buildConfigKV is
	// safe even though serve declares fewer flags than crawl. This
	// keeps the cascade identical between the two subcommands.
	for k, v := range buildConfigKV(cmd) {
		mergedKV[k] = v
	}

	cfg, err := gojira.LoadConfig(mergedKV)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return gojira.Config{}, &exitErr{code: 1, msg: "config", wrap: err}
	}
	return cfg, nil
}
