package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"

	"google.golang.org/grpc"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/grpcserver"
	"github.com/neumachen/gojira/log"
	cli "github.com/urfave/cli/v3"
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
// credentials and output settings the [grpcserver] handlers actually
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
// It mirrors [runCrawl]'s configuration cascade — file < env < flag,
// driven through the legacy [gojira.LoadConfig] validation pass so the
// user-facing error surface stays identical — then loads the server
// bind address through the App cascade, opens a TCP listener, and
// registers the [grpcserver] implementation on a fresh [grpc.Server].
//
// Shutdown is driven by ctx, which run() wires to SIGINT/SIGTERM via
// the same signalled atomic the crawl command uses. A clean
// signal-driven shutdown returns nil (exit code 0): a long-running
// server that exits cleanly when asked to is the expected lifecycle,
// not a partial success. Only [grpc.Server.Serve] returning a non-nil
// error produces a non-zero exit.
func runServe(ctx context.Context, cmd *cli.Command, env map[string]string, signalled *atomic.Bool) error {
	stderr := cmd.Root().ErrWriter
	if stderr == nil {
		stderr = os.Stderr
	}

	cfg, err := loadServeConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	addr, err := resolveServerAddress(cmd, env, stderr)
	if err != nil {
		return err
	}

	logger := newServeLogger(cfg, stderr)
	// The logger is built primarily for symmetry with runCrawl and to
	// emit the startup line at the same level the rest of the binary
	// uses. The grpcserver implementation does not currently take a
	// logger directly; once it does the same logger should be threaded
	// through to it. Silencing the unused-variable warning by emitting
	// the startup event here keeps that future hand-off natural.
	logger.Info("gojira serve starting",
		slog.String("address", addr),
		slog.String("output_dir", cfg.OutputDir))

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(stderr, "error: listen on %s: %v\n", addr, err)
		return &exitErr{code: 1, msg: "listen", wrap: err}
	}

	grpcServer := grpc.NewServer()
	gojirav1.RegisterGojiraServer(grpcServer, grpcserver.NewServer(cfg))

	// Report the *actually-bound* address, which differs from `addr`
	// when the caller asks for ":0" / "127.0.0.1:0" (ephemeral). Tests
	// and process supervisors rely on this line being stable.
	boundAddr := lis.Addr().String()
	fmt.Fprintf(stderr, "gojira gRPC server listening on %s\n", boundAddr)

	return serveUntilDone(ctx, grpcServer, lis, stderr, signalled)
}

// serveUntilDone runs grpcServer.Serve in a goroutine and waits for
// either (1) ctx cancellation — triggering a graceful stop — or (2) the
// goroutine returning a Serve error. It returns nil on a clean
// signal-driven shutdown and an [*exitErr] only when Serve itself
// reports a failure.
func serveUntilDone(ctx context.Context, grpcServer *grpc.Server, lis net.Listener, stderr io.Writer, signalled *atomic.Bool) error {
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- grpcServer.Serve(lis)
	}()

	select {
	case <-ctx.Done():
		// Graceful stop drains in-flight RPCs, then Serve returns nil.
		grpcServer.GracefulStop()
		<-serveErrCh

		// Distinguish signal-driven shutdown (a normal lifecycle event
		// for a long-running server) from an externally-cancelled
		// context (e.g. the run() crawlCtx was cancelled by a test
		// timeout). Both return nil here — the server has done its
		// job — but we log the signal case so operators see why the
		// process is exiting.
		if signalled.Load() {
			fmt.Fprintln(stderr, "gojira gRPC server stopped (signal)")
		} else {
			fmt.Fprintln(stderr, "gojira gRPC server stopped")
		}
		return nil

	case err := <-serveErrCh:
		if err != nil {
			fmt.Fprintf(stderr, "error: serve: %v\n", err)
			return &exitErr{code: 1, msg: "serve", wrap: err}
		}
		return nil
	}
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

// resolveServerAddress consults the App cascade for the configured
// bind address and lets the --address flag (or GOJIRA_SERVER_ADDRESS
// via the flag's Sources chain) override it. An empty result falls back
// to the loopback default that [config.DefaultServerSettings] uses.
func resolveServerAddress(cmd *cli.Command, env map[string]string, stderr io.Writer) (string, error) {
	srvCfg, err := gojira.LoadServerConfig(cmd.String("config"), env)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return "", &exitErr{code: 1, msg: "server config", wrap: err}
	}
	addr := srvCfg.Address
	if cmd.IsSet("address") {
		addr = cmd.String("address")
	}
	if addr == "" {
		addr = "127.0.0.1:50051"
	}
	return addr, nil
}

// newServeLogger builds the slog logger the serve startup line uses.
// The level / format choice mirrors runCrawl exactly so a single
// configuration file produces identical log output for both
// subcommands.
func newServeLogger(cfg gojira.Config, stderr io.Writer) *slog.Logger {
	format, _ := log.ParseFormat(cfg.LogFormat)
	var slevel slog.Level
	_ = slevel.UnmarshalText([]byte(strings.ToUpper(cfg.LogLevel)))
	return log.New(format, slevel, stderr)
}
