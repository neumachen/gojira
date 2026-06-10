package config

// MCPSettings configures the gojira Model Context Protocol server
// (`gojira mcp`). It is one of the standalone entity structs composed
// under [App]; it can also be used directly by any component that
// only needs the MCP knobs.
//
// The env keys carry the full GOJIRA_MCP_* names so the parent App
// can use `env:",nested"` (empty prefix) without introducing nested-
// prefix stutter such as GOJIRA_MCP_MCP_*.
//
// # Why Mode has no embedded default
//
// Mode is REQUIRED by `gojira mcp` at startup (it selects between
// the in-process facade backend ("self") and the gRPC-bridge backend
// ("bridge")), but it must NOT be required for any other command.
// crawl/serve configs that have no `mcp:` section must keep loading
// without error. Mirroring that policy at the cascade level, the
// embedded default is intentionally the empty string and the JSON
// Schema marks the `mcp` object — including its `mode` field — as
// optional. The `gojira mcp` command itself enforces presence and
// the {self,bridge} enum at startup; LoadConfig stays unchanged.
type MCPSettings struct {
	// Mode selects the MCP server backend. One of:
	//   - "self"   — run the gojira facade in-process.
	//   - "bridge" — forward each tool call to a running
	//                `gojira serve` gRPC server at server.address.
	// Required at `gojira mcp` startup; empty / absent for other
	// commands. Sourced from GOJIRA_MCP_MODE or the "mcp.mode" YAML
	// key. Default: "" (no default by design — see type doc).
	//
	// `omitempty` on the yaml tag is load-bearing: when Mode is the
	// zero value the field MUST NOT appear in marshaled YAML so the
	// schema's mode-enum check ({self,bridge}) does not reject a
	// default-shaped config (which `gojira init` writes today). With
	// omitempty, an unset Mode is simply absent — exactly matching
	// the not-required-split semantics enforced by the schema.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty" env:"GOJIRA_MCP_MODE"`

	// AllowWrites gates the mutating MCP tools (create_issue,
	// update_issue, add_comment, transition_issue). When false
	// (the default), those tools are ABSENT from the MCP server's
	// tools/list response so an AI host cannot mutate Jira at all
	// until the operator opts in. When true, the four write tools
	// are registered alongside the read tools. Sourced from
	// GOJIRA_MCP_ALLOW_WRITES or the "mcp.allow_writes" YAML key.
	// Default: false.
	AllowWrites bool `yaml:"allow_writes" json:"allow_writes" env:"GOJIRA_MCP_ALLOW_WRITES"`
}

// DefaultMCPSettings returns the embedded MCP defaults. Mode is left
// empty on purpose: `gojira mcp` requires the user to declare it
// explicitly (see the type-level doc on [MCPSettings]). AllowWrites
// defaults to false so the safe, read-only tool set is what a fresh
// configuration yields.
func DefaultMCPSettings() MCPSettings {
	return MCPSettings{
		Mode:        "",
		AllowWrites: false,
	}
}
