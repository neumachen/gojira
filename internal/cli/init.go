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
package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/neumachen/gojira/internal/config"
	urfave "github.com/urfave/cli/v3"
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

// initCommand returns the *urfave.Command for "gojira init". It mirrors
// the env-source pattern used by crawlFlags/connFlags so a partially
// configured environment still drives init.
func initCommand(env map[string]string) *urfave.Command {
	src := func(key string) urfave.ValueSourceChain {
		return urfave.NewValueSourceChain(newMapValueSource(env, key))
	}
	return &urfave.Command{
		Name:      "init",
		Usage:     "Create a gojira config file",
		ArgsUsage: " ", // no positional args
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "site", Usage: "Jira Cloud base URL",
				Sources: src("GOJIRA_SITE")},
			&urfave.StringFlag{Name: "user", Usage: "Atlassian account email",
				Sources: src("GOJIRA_USER")},
			&urfave.StringFlag{Name: "token", Usage: "Atlassian API token",
				Sources: src("GOJIRA_TOKEN")},
			&urfave.StringFlag{Name: "output-dir", Usage: "Output root directory",
				Sources: src("GOJIRA_OUTPUT_DIR")},
			&urfave.StringFlag{Name: "server-address", Usage: "gRPC server bind address",
				Sources: src("GOJIRA_SERVER_ADDRESS")},
			&urfave.BoolFlag{Name: "force", Usage: "Overwrite an existing config file"},
			&urfave.BoolFlag{Name: "global", Usage: "Write the global config (~/.config/gojira/config.yaml); this is the default"},
			&urfave.BoolFlag{Name: "local", Usage: "Write a project-local ./gojira.yaml in the current directory instead of the global config"},
		},
		Action: func(ctx context.Context, cmd *urfave.Command) error {
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
func runInit(ctx context.Context, cmd *urfave.Command, env map[string]string) error {
	stderr := guardStderr(cmd)
	stdout := initStdout(cmd)

	// (a) Resolve the target path. Default target is the global XDG
	// config; --local switches to a project-local ./gojira.yaml in
	// the current working directory. The two flags are mutually
	// exclusive to make the chosen target unambiguous on the
	// command line. The local file is written COMPLETE (same fields
	// as the global one), so it is self-sufficient and does not
	// depend on a global config existing.
	wantGlobal := cmd.Bool("global")
	wantLocal := cmd.Bool("local")
	if wantGlobal && wantLocal {
		fmt.Fprintln(stderr, "error: --global and --local are mutually exclusive; pick one")
		return &exitErr{code: 1, msg: "global and local are mutually exclusive"}
	}

	var (
		path    string
		isLocal bool
		cwd     string
	)
	if wantLocal {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(stderr, "error: resolve working directory: %v\n", err)
			return &exitErr{code: 1, msg: "getwd", wrap: err}
		}
		path = filepath.Join(cwd, config.LocalConfigFileName)
		isLocal = true
	} else {
		// Default OR explicit --global: same resolver the guard
		// uses. An empty result means we cannot proceed without
		// guessing — bail with a clear message.
		path = config.NewDefaultXDGResolver().GlobalConfigFile()
		if path == "" {
			fmt.Fprintln(stderr, "error: could not resolve global config path: set HOME or XDG_CONFIG_HOME")
			return &exitErr{code: 1, msg: "no config path"}
		}
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

	// (g) .gitignore safety nudge — local target only. The file
	// contains a Jira API token in a project directory; it is easy
	// to accidentally commit. We append a gojira.yaml entry to an
	// existing .gitignore, or warn loudly when no .gitignore is
	// present. Failures here are NON-FATAL: the config WAS written.
	if isLocal {
		if err := ensureGitignored(cwd, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "warning: update .gitignore: %v\n", err)
		}
	}

	// (h) Success line.
	fmt.Fprintf(stdout, "wrote config to %s\n", path)
	return nil
}

// ensureGitignored adds a "gojira.yaml" entry to ./.gitignore when one
// is missing, or warns to stderr if no .gitignore exists. It is the
// safety net for `gojira init --local`, whose output file contains a
// Jira API token. Behaviour:
//
//   - .gitignore exists AND already lists "gojira.yaml" (line-exact,
//     after TrimSpace): no-op, no message.
//   - .gitignore exists but does NOT list "gojira.yaml": append the
//     entry (preceded by a newline when the file does not end in
//     one) and print "added gojira.yaml to .gitignore" to stdout.
//   - .gitignore is absent: do NOT create it; print a warning to
//     stderr instead, leaving the choice (and the file) to the user.
//
// Read/write failures are returned for the caller to log as warnings;
// they are NOT fatal because the config was already written.
func ensureGitignored(cwd string, stdout, stderr io.Writer) error {
	gitignorePath := filepath.Join(cwd, ".gitignore")
	info, err := os.Stat(gitignorePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr,
				"warning: ./gojira.yaml contains your Jira API token; add it to .gitignore to avoid committing it")
			return nil
		}
		return err
	}
	if info.IsDir() {
		// A directory named .gitignore is a user-environment
		// oddity, not our problem to fix; surface it as a
		// warning so the user can investigate.
		fmt.Fprintln(stderr,
			"warning: ./.gitignore is a directory; cannot add gojira.yaml entry")
		return nil
	}

	body, err := os.ReadFile(gitignorePath)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == config.LocalConfigFileName {
			return nil // already ignored — silent no-op
		}
	}

	// Append the entry. Ensure a separating newline before the new
	// line when the existing file does not end in one, so the entry
	// lands on its own line and the file stays POSIX-clean.
	var toAppend []byte
	if len(body) > 0 && body[len(body)-1] != '\n' {
		toAppend = append(toAppend, '\n')
	}
	toAppend = append(toAppend, []byte(config.LocalConfigFileName+"\n")...)

	f, err := os.OpenFile(gitignorePath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, werr := f.Write(toAppend); werr != nil {
		_ = f.Close()
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		return cerr
	}
	fmt.Fprintln(stdout, "added gojira.yaml to .gitignore")
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// initStdout returns the cmd's resolved stdout, falling back to
// os.Stdout — mirroring stdoutOf in write_cmds.go.
func initStdout(cmd *urfave.Command) io.Writer {
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
