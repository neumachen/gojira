package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	gojira "github.com/neumachen/gojira"
)

// ErrModeRequired is returned by [Serve] when cfg.MCPMode is the empty
// string. The cmd layer maps it to exit code 1; library callers can
// use errors.Is to distinguish the user-config failure from a real
// runtime/protocol error.
var ErrModeRequired = errors.New("mcp: mcp.mode is required (self|bridge)")

// ErrInvalidMode is returned by [Serve] when cfg.MCPMode is non-empty
// but not one of {ModeSelf, ModeBridge}. The cmd layer maps it to
// exit code 1.
var ErrInvalidMode = errors.New("mcp: invalid mcp.mode (must be self|bridge)")

// Serve runs the gojira MCP server over stdio until ctx is cancelled
// (clean shutdown) or a protocol/transport error occurs.
//
// cfg is assumed already validated by the caller's config cascade.
// Serve enforces the mcp.mode invariant itself (self|bridge) because
// LoadConfig stays neutral about it; an unset/invalid mode is a user
// configuration error surfaced as a returned error (wrapping
// [ErrModeRequired] or [ErrInvalidMode]), NOT a panic.
//
// For bridge mode the dial target is cfg.ServerAddress — phase 2's
// config cascade always populates it (default 127.0.0.1:50051), so
// Serve never invents an address of its own. In self mode the address
// is unused; the startup line still reports it as empty.
//
// STDOUT-PURITY INVARIANT: Serve and everything it calls MUST NOT
// write a single byte to stdout — stdout is reserved for the MCP
// JSON-RPC stream owned by the SDK's StdioTransport. Every diagnostic
// (mode-required, invalid mode, backend-build failure, startup line,
// terminal serve error) goes to os.Stderr.
//
// Shutdown semantics: when the stdio run returns AND ctx is cancelled,
// Serve treats it as a clean signal-driven shutdown and returns nil.
// Any other non-nil run error is written to stderr and returned.
func Serve(ctx context.Context, cfg gojira.Config) error {
	// (c) Mode enforcement. The exact stderr messages match the
	// pre-phase-3 cmd-layer wording so existing tests (and any
	// operator-facing documentation) continue to see the same
	// human-readable surface.
	mode := cfg.MCPMode
	switch mode {
	case "":
		fmt.Fprintln(os.Stderr, "error: mcp.mode is required (self|bridge); set it in config or GOJIRA_MCP_MODE")
		return ErrModeRequired
	case ModeSelf, ModeBridge:
		// ok
	default:
		fmt.Fprintf(os.Stderr, "error: invalid mcp.mode %q (must be self|bridge)\n", mode)
		return fmt.Errorf("%w: %q", ErrInvalidMode, mode)
	}

	// (d) Bridge target. In self mode an unused address would be
	// misleading on the startup line; report it as empty.
	var serverAddr string
	if mode == ModeBridge {
		serverAddr = cfg.ServerAddress
	}

	// (e) Backend construction. NewBridgeBackend's grpc.NewClient is
	// lazy — a dial failure surfaces at first RPC, not here.
	backend, closer, err := NewBackend(cfg, mode, serverAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: build mcp backend: %v\n", err)
		return fmt.Errorf("mcp: build backend: %w", err)
	}
	defer func() { _ = closer() }()

	// (f) Startup line on STDERR. A slog TextHandler matches the
	// pre-phase-3 surface so log parsers keyed on the "gojira mcp
	// starting" line keep working unchanged. The MCP SDK does not
	// consume this logger directly; the line is the only routine
	// human-readable output.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("gojira mcp starting",
		slog.String("mode", mode),
		slog.Bool("allow_writes", cfg.MCPAllowWrites),
		slog.String("server_address", serverAddr))

	// (g) Build the MCP server with the gated tool set.
	srv := NewMCPServer(backend, cfg.MCPAllowWrites)

	// (h) Stdio serve + clean shutdown. The SDK's Run loop honors
	// ctx — a SIGINT reaches us via run()'s signal handling and
	// cancels the context, which we treat as a clean shutdown
	// signal regardless of the run loop's error return.
	if err := runStdio(ctx, srv); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "error: mcp serve: %v\n", err)
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}
