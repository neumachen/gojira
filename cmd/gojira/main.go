// Command gojira is the CLI binary for the gojira Jira-to-Markdown mirror
// tool. It is a thin consumer of the gojira library facade: it parses flags
// via urfave/cli/v3, loads environment variables (passed in by main as a
// map for test injectability), constructs an events sink that formats to
// stderr, calls the library, handles OS signals, and translates the
// returned summary into an exit code.
//
// It does NOT add any capability the library does not already expose.
// All command wiring lives in [github.com/neumachen/gojira/internal/cli];
// this file is intentionally minimal so the entrypoint stays trivial.
//
// Usage:
//
//	gojira [--help] [--version]
//	gojira crawl [flags] <ISSUE-KEY>
//
// Required configuration (flag or env var):
//
//	--site / GOJIRA_SITE              Jira Cloud base URL
//	--user / GOJIRA_USER              Atlassian account email
//	--token / GOJIRA_TOKEN            Atlassian API token
//	--output-dir / GOJIRA_OUTPUT_DIR  Output root directory
//
// Optional configuration:
//
//	--depth-limit / GOJIRA_DEPTH_LIMIT        (default 0 = unlimited)
//	--issue-cap / GOJIRA_ISSUE_CAP            (default 500)
//	--time-cap / GOJIRA_TIME_CAP_SECONDS      (default 0 = unlimited)
//	--concurrency / GOJIRA_CONCURRENCY        (default 3)
//	--refetch / GOJIRA_REFETCH                (default false)
//	--include-comments / GOJIRA_INCLUDE_COMMENTS (default false)
//	--log-level / GOJIRA_LOG_LEVEL            (default info)
//	--log-format / GOJIRA_LOG_FORMAT          (default text)
//	--include-children / GOJIRA_INCLUDE_CHILDREN (default true)
//	--child-search-limit / GOJIRA_CHILD_SEARCH_LIMIT (default 100)
//	--epic-link-field / GOJIRA_EPIC_LINK_FIELD   (default auto-detect)
//	--include-dev-status / GOJIRA_INCLUDE_DEV_STATUS (default true)
//	--dev-status-applications / GOJIRA_DEV_STATUS_APPLICATIONS (default GitHub)
//	--dev-status-data-types / GOJIRA_DEV_STATUS_DATA_TYPES (default pullrequest,branch,commit,repository,build)
//	--render-null-custom-fields / GOJIRA_RENDER_NULL_CUSTOM_FIELDS (default false)
//
// Exit codes:
//
//	0  All issues fetched successfully (no failures, stubs, or cap-limits).
//	1  Total failure: auth error, config error, or nothing was rendered.
//	2  Partial success: at least one issue rendered but some failed or were
//	   cap-limited; also used when the crawl was interrupted by a signal.
//	130 Force-quit by second SIGINT/SIGTERM (POSIX convention).
package main

import (
	"context"
	"os"

	"github.com/neumachen/gojira/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args, os.Stdout, os.Stderr, cli.EnvMap()))
}
