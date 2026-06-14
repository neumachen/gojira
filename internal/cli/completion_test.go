// completion_test.go — tests for the urfave/cli/v3 built-in shell
// completion subcommand wired up via EnableShellCompletion=true on
// buildRootCommand. The actual scripts are templated by urfave; we
// don't re-test urfave, just the wiring:
//
//   - `gojira completion <shell>` exits 0 for each supported shell;
//   - emits a non-empty script that interpolates the program name
//     (the most stable substring across all shells);
//   - works with NO config file and NO GOJIRA_* env present — i.e.
//     the requireConfig guard does NOT block completion (the guard
//     runs per-command inside Jira-touching Actions, so the auto-
//     injected `completion` command is naturally exempt).
//
// Per-shell content sniffs use the most-stable urfave template
// markers we observed in v3.9.0 outputs — robust against the
// scripts evolving (banner text, helper names) so future urfave
// bumps don't make the test brittle.
package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompletion_EmitsScriptsForEachShell verifies the auto-injected
// `completion` subcommand emits a non-empty, shell-appropriate
// script for each supported shell. The per-shell substring is a
// stable marker present in the urfave template; if a future bump
// renames the marker, switch to the lowest-common-denominator
// "contains the program name" assertion alone.
func TestCompletion_EmitsScriptsForEachShell(t *testing.T) {
	cases := []struct {
		shell  string
		needle string // stable marker present in the urfave template
	}{
		// bash script defines a __gojira_init_completion helper
		// (urfave interpolates the program name into the helper
		// name).
		{shell: "bash", needle: "__gojira_init_completion"},
		// zsh script begins with the standard zsh completion
		// preamble pinning the command name.
		{shell: "zsh", needle: "#compdef gojira"},
		// fish script defines a __gojira_perform_completion
		// function as the dispatch entry point.
		{shell: "fish", needle: "__gojira_perform_completion"},
		// pwsh script registers a native argument completer; this
		// line is shell-independent of the program name and the
		// most-stable structural marker in the script.
		{shell: "pwsh", needle: "Register-ArgumentCompleter"},
	}
	for _, tc := range cases {
		t.Run(tc.shell, func(t *testing.T) {
			// Empty env: no GOJIRA_* keys, no config-file pointer.
			stdout, stderr, code := captureRun(context.Background(),
				[]string{"gojira", "completion", tc.shell},
				map[string]string{})
			require.Equal(t, 0, code,
				"`gojira completion %s` must exit 0; stderr=%q",
				tc.shell, stderr)
			require.NotEmpty(t, stdout,
				"`gojira completion %s` must emit a non-empty script",
				tc.shell)

			// Per-shell structural marker. Documented in the case
			// literal so a future urfave bump that renames the
			// helper produces a focused diff instead of a mystery
			// failure. Note that the pwsh template does NOT embed
			// the program name (it resolves $MyInvocation at
			// PowerShell runtime), so we deliberately do not
			// assert "contains 'gojira'" globally — the structural
			// marker is the stable contract.
			assert.Contains(t, stdout, tc.needle,
				"completion script for %s must contain %q",
				tc.shell, tc.needle)
		})
	}
}

// TestCompletion_GuardExempt_NoConfigNeeded asserts the completion
// command works with ZERO configuration sources — no flags, no env,
// no discoverable config file. The requireConfig guard lives inside
// Jira-touching Actions, so the auto-injected `completion` command
// is naturally exempt; this test locks that property in so a future
// "global pre-action guard" refactor does not silently break shell
// completion for first-run users (the exact moment they need it).
func TestCompletion_GuardExempt_NoConfigNeeded(t *testing.T) {
	// neutralizeXDG (defined in guard_test.go) points the XDG
	// resolver at empty tempdirs and t.Chdir's to a fresh dir so
	// arm-4 of the guard cannot accidentally resolve a stray
	// config from the host.
	neutralizeXDG(t)

	stdout, stderr, code := captureRun(context.Background(),
		[]string{"gojira", "completion", "bash"},
		map[string]string{}) // no GOJIRA_* keys at all
	require.Equal(t, 0, code,
		"completion must work with NO config; stderr=%q", stderr)
	assert.NotEmpty(t, stdout,
		"completion must emit a non-empty script even without config")

	// The guard's actionable failure message must NOT appear: a
	// future regression where requireConfig is hoisted into a
	// global pre-action would surface here as the familiar
	// "Run 'gojira init'" banner leaking onto stderr.
	assert.False(t, strings.Contains(stderr, "no gojira configuration found"),
		"guard must NOT fire for `completion`; stderr=%q", stderr)
}
