// init.go — the `gojira init` subcommand.
//
// init scaffolds a schema-valid config.yaml at the XDG global path
// ($XDG_CONFIG_HOME/gojira/config.yaml or ~/.config/gojira/config.yaml)
// with 0o600 permissions. Required values come from flags, env, or
// interactive prompts; the token is read without echo on real
// terminals (via golang.org/x/term) and falls back to a warned echo
// read when stdin is not a terminal (tests, pipes).
//
// init is intentionally NOT guarded by [requireConfig]: it is the way
// out of a no-config state and must always be runnable.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/neumachen/gojira/internal/config"
	cli "github.com/urfave/cli/v3"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Injectable seams (tests overwrite via package-level swap + t.Cleanup)
// ---------------------------------------------------------------------------

// initStdin is the line reader for non-secret prompts. Production
// reads from os.Stdin; tests inject a strings.Reader.
var initStdin io.Reader = os.Stdin

// readTokenFn is the secret-input seam. Production prefers no-echo
// via [readTokenNoEcho]; the unit-test mocking path swaps a fake
// that returns a fixed token without touching the terminal.
var readTokenFn = readTokenNoEcho

// ---------------------------------------------------------------------------
// Command construction
// ---------------------------------------------------------------------------

// initCommand returns the *cli.Command for "gojira init". It mirrors
// the env-source pattern used by crawlFlags/connFlags so a partially
// configured environment still drives init.
func initCommand(env map[string]string) *cli.Command {
	src := func(key string) cli.ValueSourceChain {
		return cli.NewValueSourceChain(newMapValueSource(env, key))
	}
	return &cli.Command{
		Name:      "init",
		Usage:     "Create a gojira config file",
		ArgsUsage: " ", // no positional args
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "site", Usage: "Jira Cloud base URL",
				Sources: src("GOJIRA_SITE")},
			&cli.StringFlag{Name: "user", Usage: "Atlassian account email",
				Sources: src("GOJIRA_USER")},
			&cli.StringFlag{Name: "token", Usage: "Atlassian API token",
				Sources: src("GOJIRA_TOKEN")},
			&cli.StringFlag{Name: "output-dir", Usage: "Output root directory",
				Sources: src("GOJIRA_OUTPUT_DIR")},
			&cli.StringFlag{Name: "server-address", Usage: "gRPC server bind address",
				Sources: src("GOJIRA_SERVER_ADDRESS")},
			&cli.BoolFlag{Name: "force", Usage: "Overwrite an existing config file"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runInit(ctx, cmd, env)
		},
	}
}

// ---------------------------------------------------------------------------
// Action
// ---------------------------------------------------------------------------

// runInit implements the init workflow described in the package doc:
// resolve the target path, refuse to clobber without --force, gather
// values from flags / env / prompts, assemble an App, marshal to YAML,
// and write 0o600.
func runInit(ctx context.Context, cmd *cli.Command, env map[string]string) error {
	stderr := guardStderr(cmd)
	stdout := initStdout(cmd)

	// (a) Resolve the target path via the same resolver used by the
	// guard. An empty result means we cannot proceed without
	// guessing — bail with a clear message.
	path := config.NewDefaultXDGResolver().GlobalConfigFile()
	if path == "" {
		fmt.Fprintln(stderr, "error: could not resolve global config path: set HOME or XDG_CONFIG_HOME")
		return &exitErr{code: 1, msg: "no config path"}
	}

	// (b) Refuse to clobber unless --force.
	if _, err := os.Stat(path); err == nil && !cmd.Bool("force") {
		fmt.Fprintf(stderr, "error: config already exists at %s; pass --force to overwrite\n", path)
		return &exitErr{code: 1, msg: "config exists"}
	}

	// (c) Gather non-secret values from flags first, prompting on stdin
	// for anything still missing. The token is gathered separately so
	// the no-echo path is the only place that reads it.
	reader := bufio.NewReader(initStdin)

	site := strings.TrimSpace(cmd.String("site"))
	if site == "" {
		var err error
		site, err = promptLine(stdout, reader, "Jira site URL: ", "")
		if err != nil {
			return &exitErr{code: 1, msg: "read site", wrap: err}
		}
	}
	user := strings.TrimSpace(cmd.String("user"))
	if user == "" {
		var err error
		user, err = promptLine(stdout, reader, "Atlassian account email: ", "")
		if err != nil {
			return &exitErr{code: 1, msg: "read user", wrap: err}
		}
	}
	serverAddress := strings.TrimSpace(cmd.String("server-address"))
	if serverAddress == "" {
		def := config.DefaultServerSettings().Address
		var err error
		serverAddress, err = promptLine(stdout, reader,
			fmt.Sprintf("gRPC server address [%s]: ", def), def)
		if err != nil {
			return &exitErr{code: 1, msg: "read server-address", wrap: err}
		}
	}

	// (d) Token: secret. If flag/env supplied a value, use it; else
	// route through the no-echo seam.
	token := cmd.String("token")
	if token == "" {
		var err error
		token, err = readTokenFn("Atlassian API token: ", stderr)
		if err != nil {
			return &exitErr{code: 1, msg: "read token", wrap: err}
		}
		token = strings.TrimSpace(token)
	}

	// output-dir: flag or default; never prompted.
	outputDir := strings.TrimSpace(cmd.String("output-dir"))
	if outputDir == "" {
		outputDir = config.DefaultOutputSettings().Dir
	}

	// Basic non-empty validation — keeps the YAML round-trip
	// meaningful for downstream LoadFileConfig users.
	for _, pair := range []struct{ name, val string }{
		{"site", site}, {"user", user}, {"token", token},
		{"server-address", serverAddress},
	} {
		if pair.val == "" {
			fmt.Fprintf(stderr, "error: %s is required (no value via flag, env, or prompt)\n", pair.name)
			return &exitErr{code: 1, msg: "missing " + pair.name}
		}
	}

	// (e) Build the App tree from defaults + the gathered values.
	app := config.DefaultApp()
	app.Jira.BaseURL = site
	app.Jira.Email = user
	app.Jira.APIToken = token
	app.Output.Dir = outputDir
	app.Server.Address = serverAddress

	body, err := marshalAppYAML(app)
	if err != nil {
		fmt.Fprintf(stderr, "error: marshal config: %v\n", err)
		return &exitErr{code: 1, msg: "marshal", wrap: err}
	}

	// (f) Write.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(stderr, "error: create config directory %s: %v\n", filepath.Dir(path), err)
		return &exitErr{code: 1, msg: "mkdir", wrap: err}
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		fmt.Fprintf(stderr, "error: write config %s: %v\n", path, err)
		return &exitErr{code: 1, msg: "write", wrap: err}
	}

	// (g) Success line.
	fmt.Fprintf(stdout, "wrote config to %s\n", path)
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// initStdout returns the cmd's resolved stdout, falling back to
// os.Stdout — mirroring stdoutOf in write_cmds.go.
func initStdout(cmd *cli.Command) io.Writer {
	if w := cmd.Root().Writer; w != nil {
		return w
	}
	return os.Stdout
}

// promptLine writes the prompt to w, reads one line from r, trims
// whitespace, and returns the line (or def when the line is empty
// after trimming). An EOF after a partial line is treated as the
// partial line; an EOF before any input returns the default.
func promptLine(w io.Writer, r *bufio.Reader, prompt, def string) (string, error) {
	if _, err := io.WriteString(w, prompt); err != nil {
		return "", err
	}
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return def, nil
	}
	return line, nil
}

// readTokenNoEcho is the production token reader. When os.Stdin is a
// real terminal, it reads without echo via golang.org/x/term. When it
// is not (test pipes, CI pipelines), it falls back to a warned echo
// read against initStdin — the same seam non-secret prompts use, so
// piped stdin works end to end and tests stay hermetic.
func readTokenNoEcho(prompt string, errOut io.Writer) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if _, err := io.WriteString(errOut, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return "", err
		}
		// term.ReadPassword does not emit a newline; nudge the
		// cursor so subsequent output starts on a fresh line.
		_, _ = io.WriteString(errOut, "\n")
		return string(b), nil
	}
	return readTokenWithEcho(prompt, initStdin, errOut)
}

// readTokenWithEcho is the non-terminal fallback. It warns the user
// that input will be visible (errOut), then reads one line from in.
// Tests use this path via initStdin = strings.NewReader(...), so the
// secret value never appears on stdout — it goes through the
// supplied reader straight into the in-memory config struct.
func readTokenWithEcho(prompt string, in io.Reader, errOut io.Writer) (string, error) {
	_, _ = fmt.Fprintln(errOut, "WARNING: reading token with echo; the input will be visible on-screen")
	if _, err := io.WriteString(errOut, prompt); err != nil {
		return "", err
	}
	r := bufio.NewReader(in)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	return strings.TrimSpace(line), nil
}

// marshalAppYAML returns the YAML body to write. A short header
// comment documents provenance and the file's secret-bearing status
// so a user opening the file later understands why it's 0600.
func marshalAppYAML(app config.App) ([]byte, error) {
	body, err := yaml.Marshal(app)
	if err != nil {
		return nil, err
	}
	const header = "# Generated by `gojira init`. Contains a Jira API token — keep it secret (file mode 0600).\n"
	out := make([]byte, 0, len(header)+len(body))
	out = append(out, header...)
	out = append(out, body...)
	return out, nil
}
