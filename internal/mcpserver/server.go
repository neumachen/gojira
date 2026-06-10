package mcpserver

// SDK symbols locked from `go doc github.com/modelcontextprotocol/go-sdk/mcp`
// at v1.6.1 (commit verified against go.sum). Future maintainers should
// re-run `go doc` after any SDK bump and re-verify these signatures.
//
//   mcp.NewServer(impl *Implementation, options *ServerOptions) *Server
//   mcp.AddTool[In, Out any](s *Server, t *Tool, h ToolHandlerFor[In, Out])
//     where ToolHandlerFor[In, Out any] =
//           func(context.Context, *CallToolRequest, In) (*CallToolResult, Out, error)
//   (*Server).Run(ctx context.Context, t Transport) error
//   (*Server).Connect(ctx, Transport, *ServerSessionOptions) (*ServerSession, error)
//   mcp.NewInMemoryTransports() (*InMemoryTransport, *InMemoryTransport)
//   mcp.NewClient(impl *Implementation, opts *ClientOptions) *Client
//   (*Client).Connect(ctx, Transport, *ClientSessionOptions) (*ClientSession, error)
//   (*ClientSession).ListTools(ctx, *ListToolsParams) (*ListToolsResult, error)
//   (*ClientSession).CallTool(ctx, *CallToolParams) (*CallToolResult, error)
//   (*ServerSession).NotifyProgress(ctx, *ProgressNotificationParams) error
//   CallToolRequest = ServerRequest[*CallToolParamsRaw]
//     req.Params.GetProgressToken() any  — nil when client did not supply one
//     req.Session *ServerSession
//   mcp.StdioTransport — concrete struct, instantiate with &mcp.StdioTransport{}
//
// Progress / Errors:
//   - Tool handler returning a non-nil error is auto-wrapped into a
//     CallToolResult with IsError=true and text content. Our handlers
//     prefer explicit errorResult(err) so the sentinel category prefix
//     reaches the host LLM verbatim.

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/neumachen/gojira"
)

// ModeSelf is the cfg.MCPMode value selecting the in-process facade
// backend.
const ModeSelf = "self"

// ModeBridge is the cfg.MCPMode value selecting the gRPC-bridge
// backend. The address comes from the cmd layer (resolved through
// the App.ServerSettings cascade).
const ModeBridge = "bridge"

// NewMCPServer constructs an [*mcp.Server] for the gojira MCP surface.
// It calls mcp.NewServer with an Implementation identifying the server
// and version, then registers tools via [registerTools]. The read
// tools are always present; the four write tools are only registered
// when allowWrites is true.
func NewMCPServer(b mcpBackend, allowWrites bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "gojira",
		Version: gojira.Version,
	}, nil)
	registerTools(server, b, allowWrites)
	return server
}

// NewBackend resolves the configured mode and returns the matching
// [mcpBackend], a closer the caller must defer (no-op for self, gRPC
// conn.Close for bridge), and any construction error.
//
// mode must be one of "self" or "bridge"; an empty or unknown mode
// returns an error so the cmd layer can surface a clear startup
// failure. The cmd layer is expected to have already validated the
// mode against the user-facing message — this is paranoid validation
// at the boundary.
//
// For bridge mode, serverAddr is the gRPC dial target (typically
// resolved from the App.Server.Address cascade in the cmd layer).
func NewBackend(cfg gojira.Config, mode, serverAddr string) (mcpBackend, func() error, error) {
	switch mode {
	case ModeSelf:
		return NewFacadeBackend(cfg), func() error { return nil }, nil
	case ModeBridge:
		bb, closer, err := NewBridgeBackend(serverAddr)
		if err != nil {
			return nil, nil, err
		}
		return bb, closer, nil
	case "":
		return nil, nil, errors.New("mcpserver: mode is required (self|bridge)")
	default:
		return nil, nil, fmt.Errorf("mcpserver: unknown mode %q (expected self|bridge)", mode)
	}
}

// Serve runs the supplied MCP server over the stdio transport until
// ctx is cancelled or stdin is closed. It is a thin wrapper over
// (*mcp.Server).Run with the concrete *mcp.StdioTransport — kept as
// a one-liner so the cmd layer can mock it or replace the transport
// for tests without re-implementing the run loop.
func Serve(ctx context.Context, s *mcp.Server) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}
