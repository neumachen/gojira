// guard.go — the require-config pre-flight guard.
//
// Every Jira-touching subcommand (crawl, serve, create, update, comment,
// transitions, transition) calls [requireConfig] BEFORE its existing
// argument validation and config-cascade work. The guard answers a single
// question: does the caller have ANY plausible configuration source? If
// yes, the existing LoadConfig cascade runs unchanged and remains the
// sole authority on config validity. If no, the command fails fast with
// an actionable message that points at `gojira init`.
//
// init, help, and --version are NOT guarded.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/neumachen/gojira/internal/config"
	urfave "github.com/urfave/cli/v3"
)

// requireConfig is the pre-flight check guarding every Jira-touching
// command. It is pure (no I/O beyond the resolver call and the
// stderr write on failure) and idempotent — safe to call multiple
// times.
//
// Pass-arms (any one is sufficient):
//
//  1. `--config <path>` flag is set (the user supplied an explicit config).
//  2. `--site` AND `--user` AND `--token` flags are ALL set (the user
//     supplied the full credential trio inline).
//  3. The injected env map carries `GOJIRA_CONFIG_FILE` non-empty, OR
//     all three of `GOJIRA_SITE`, `GOJIRA_USER`, `GOJIRA_TOKEN` non-empty.
//  4. The default XDGResolver discovers a config file via the documented
//     chain ($GOJIRA_CONFIG_FILE -> ./gojira.yaml -> XDG global config).
//
// When NONE hold, requireConfig writes the actionable failure message
// to the command's stderr (cmd.Root().ErrWriter, falling back to
// os.Stderr — matching runServe) and returns [*exitErr] with code 1.
// The caller's existing config-cascade code is never reached.
func requireConfig(cmd *urfave.Command, env map[string]string) error {
	// Arm 1: --config flag.
	if cmd.IsSet("config") {
		return nil
	}

	// Arm 2: --site + --user + --token flags.
	if cmd.IsSet("site") && cmd.IsSet("user") && cmd.IsSet("token") {
		return nil
	}

	// Arm 3: env. The guard reads the INJECTED env map for the same
	// reason the rest of cmd/gojira does — testability, and consistency
	// with the mapValueSource pattern. The downstream LoadConfig
	// cascade reads the same map.
	if env["GOJIRA_CONFIG_FILE"] != "" {
		return nil
	}
	if env["GOJIRA_SITE"] != "" && env["GOJIRA_USER"] != "" && env["GOJIRA_TOKEN"] != "" {
		return nil
	}

	// Arm 4: file discovery via the real XDG/cwd chain. The resolver
	// reads the PROCESS env and os.Getwd — tests neutralize those
	// via t.Setenv + t.Chdir so this call is deterministic.
	if _, found := config.NewDefaultXDGResolver().DiscoverConfigFile(""); found {
		return nil
	}

	// All four arms failed → print the actionable message and bail
	// with exit code 1.
	stderr := guardStderr(cmd)
	fmt.Fprint(stderr,
		"error: no gojira configuration found.\n"+
			"\n"+
			"Run 'gojira init' to create one, or provide configuration via one of:\n"+
			"  - the --config <path> flag\n"+
			"  - the --site, --user, and --token flags together\n"+
			"  - the GOJIRA_CONFIG_FILE env var, or GOJIRA_SITE + GOJIRA_USER + GOJIRA_TOKEN env vars\n")
	return &exitErr{code: 1, msg: "config required"}
}

// guardStderr returns the stderr writer for the guard's failure
// message. It mirrors the fallback pattern used by runServe.
func guardStderr(cmd *urfave.Command) io.Writer {
	if w := cmd.Root().ErrWriter; w != nil {
		return w
	}
	return os.Stderr
}
