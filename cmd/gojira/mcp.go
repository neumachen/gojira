// mcp.go — the `gojira mcp` subcommand.
//
// runMCP is intentionally thin: apply the Phase-A require-config
// guard, load the same Config cascade that runServe uses, enforce
// mcp.mode at startup (the deliberate Phase-B split — required by
// THIS command only, never by LoadConfig globally), build the
// backend selected by mode, and serve the MCP protocol over stdio.
//
// STDIO PURITY INVARIANT — this file MUST NOT write a single byte to
// stdout. stdout is reserved for the MCP JSON-RPC stream owned by
// the SDK's StdioTransport. Every diagnostic, every error, every
// log record goes to stderr. The startup line below is the only
// human-readable output and lives on stderr.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	cli "github.com/urfave/cli/v3"

	"github.com/neumachen/gojira/internal/mcpserver"
)

// mcpCommand returns the *cli.Command for "gojira mcp". The flag set
// is intentionally the serve-style connection set so a single
// configured environment drives crawl, serve, and mcp uniformly.
// The signalled atomic is threaded so a SIGINT during a long-running
// stdio session is observed and mapped consistently with the other
// long-running command (serve).
func mcpCommand(env map[string]string, signalled *atomic.Bool) *cli.Command {
	return &cli.Command{
		Name:      "mcp",
		Usage:     "Run the gojira MCP server over stdio",
		ArgsUsage: " ", // no positional args
		Flags:     serveFlags(env),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runMCP(ctx, cmd, env, signalled)
		},
	}
}

// runMCP is the Action body for `gojira mcp`. It mirrors runServe's
// structure but swaps the grpc.Server bootstrap for the mcp server +
// stdio transport.
func runMCP(ctx context.Context, cmd *cli.Command, env map[string]string, signalled *atomic.Bool) error {
	stderr := cmd.Root().ErrWriter
	if stderr == nil {
		stderr = os.Stderr
	}

	// (a) Phase-A guard. requireConfig prints its own actionable
	// "gojira init" message to stderr on failure.
	if err := requireConfig(cmd, env); err != nil {
		return err
	}

	// (b) Same Config cascade as serve. loadServeConfig prints any
	// loader errors to stderr already.
	cfg, err := loadServeConfig(cmd, env, stderr)
	if err != nil {
		return err
	}

	// (c) Enforce mcp.mode (THIS command only — LoadConfig stays
	// neutral so crawl/serve configs without an mcp section still
	// validate).
	mode := cfg.MCPMode
	switch mode {
	case "":
		fmt.Fprintln(stderr, "error: mcp.mode is required (self|bridge); set it in config or GOJIRA_MCP_MODE")
		return &exitErr{code: 1, msg: "mcp.mode required"}
	case mcpserver.ModeSelf, mcpserver.ModeBridge:
		// ok
	default:
		fmt.Fprintf(stderr, "error: invalid mcp.mode %q (must be self|bridge)\n", mode)
		return &exitErr{code: 1, msg: "invalid mcp.mode"}
	}

	// (d) Resolve the gRPC bridge target only when in bridge mode.
	// In self mode an unused server address would be misleading on
	// the startup line. The config cascade owns the default
	// (127.0.0.1:50051) and the GOJIRA_SERVER_ADDRESS / --address
	// overrides, so cfg.ServerAddress is always populated here.
	var serverAddr string
	if mode == mcpserver.ModeBridge {
		serverAddr = cfg.ServerAddress
	}

	// (e) Build the backend. For bridge mode NewBridgeBackend's
	// grpc.NewClient is lazy — a dial failure surfaces at first RPC,
	// not here. That matches gojira-client's behavior.
	backend, closer, err := mcpserver.NewBackend(cfg, mode, serverAddr)
	if err != nil {
		fmt.Fprintf(stderr, "error: build mcp backend: %v\n", err)
		return &exitErr{code: 1, msg: "mcp backend", wrap: err}
	}
	defer func() { _ = closer() }()

	// (f) Build the slog logger over STDERR (NEVER stdout). The
	// MCP SDK does not consume this logger directly today; we use it
	// for the startup line and to keep a place ready for tool-level
	// instrumentation in a future phase.
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("gojira mcp starting",
		slog.String("mode", mode),
		slog.Bool("allow_writes", cfg.MCPAllowWrites),
		slog.String("server_address", serverAddr))

	// (g) Build the MCP server with the gated tool set.
	srv := mcpserver.NewMCPServer(backend, cfg.MCPAllowWrites)

	// (h) Serve over stdio. The SDK's Run loop honors ctx — a SIGINT
	// reaches us via run()'s signal handling and cancels the crawl
	// context, which Serve treats as a clean shutdown signal.
	if err := mcpserver.Serve(ctx, srv); err != nil {
		// A clean signal-driven shutdown should not surface as an
		// error here; only a real protocol/transport error counts.
		if ctx.Err() != nil || signalled != nil && signalled.Load() {
			return nil
		}
		fmt.Fprintf(stderr, "error: mcp serve: %v\n", err)
		return &exitErr{code: 1, msg: "mcp serve", wrap: err}
	}
	return nil
}
