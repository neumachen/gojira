// mcp.go — the `gojira mcp` subcommand.
//
// runMCP is intentionally thin: apply the Phase-A require-config guard,
// load the Config cascade shared with runServe, and hand the validated
// Config to internal/mcp.Serve, which owns mode enforcement, backend
// construction, the MCP server build, the stdio transport, and clean
// shutdown.
//
// STDOUT PURITY INVARIANT — this file MUST NOT write a single byte to
// stdout. stdout is reserved for the MCP JSON-RPC stream owned by the
// SDK's StdioTransport. The invariant is now enforced inside
// internal/mcp.Serve (every diagnostic goes to os.Stderr), but it
// remains a property of the cmd wiring too: requireConfig and
// loadServeConfig only write to the cli ErrWriter, and the only
// post-Serve cmd output is the *exitErr mapping below.
package main

import (
	"context"
	"os"
	"sync/atomic"

	cli "github.com/urfave/cli/v3"

	gojiramcp "github.com/neumachen/gojira/internal/mcp"
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
// structure: guard, load config cascade, hand off to the package's
// encapsulated Serve. A clean signal-driven shutdown is already
// nil-returned by [gojiramcp.Serve]; any real error maps to exit 1.
func runMCP(ctx context.Context, cmd *cli.Command, env map[string]string, signalled *atomic.Bool) error {
	_ = signalled // mirrored from runServe for API consistency; Serve treats ctx cancel as clean shutdown
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

	if err := gojiramcp.Serve(ctx, cfg); err != nil {
		// Serve has already written a user-facing message to os.Stderr;
		// don't echo it on the cli ErrWriter, just map to the exit code.
		return &exitErr{code: 1, msg: "mcp", wrap: err}
	}
	return nil
}
