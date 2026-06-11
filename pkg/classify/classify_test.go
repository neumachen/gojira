package classify_test

import (
	"testing"

	"github.com/neumachen/gojira/pkg/classify"
	"github.com/stretchr/testify/assert"
)

// jiraSite is the placeholder Jira Cloud site used throughout the tests.
// No real Atlassian domain is used.
const jiraSite = "https://your-site.atlassian.net"

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		site     string
		wantKind classify.Kind
		// Expected field values; zero value means "not set / don't check".
		wantIssueKey string
		wantURL      string
		wantOwner    string
		wantRepo     string
		wantPRNumber int
	}{
		// ── Bare Jira issue keys (KindJiraKey) ──────────────────────────────

		{
			name:         "valid bare key EXAMPLE-1",
			input:        "EXAMPLE-1",
			site:         jiraSite,
			wantKind:     classify.KindJiraKey,
			wantIssueKey: "EXAMPLE-1",
		},
		{
			name:         "valid bare key ABCD-12345",
			input:        "ABCD-12345",
			site:         jiraSite,
			wantKind:     classify.KindJiraKey,
			wantIssueKey: "ABCD-12345",
		},
		{
			name:         "valid bare key with digit in project A1-1",
			input:        "A1-1",
			site:         jiraSite,
			wantKind:     classify.KindJiraKey,
			wantIssueKey: "A1-1",
		},
		// Note: the issue-key pattern is [A-Z][A-Z0-9]+-[0-9]+, which requires
		// at least two characters before the hyphen (one [A-Z] + one [A-Z0-9]).
		// A single-letter project key like "A-99" does NOT match; it is External.
		{
			name:     "single-letter project key A-99 is not a valid key",
			input:    "A-99",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},

		// ── Invalid bare keys → KindExternal ────────────────────────────────

		{
			name:     "invalid key lowercase example-1",
			input:    "example-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key mixed case Example-1",
			input:    "Example-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key no number EXAMPLE-",
			input:    "EXAMPLE-",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key no project -1",
			input:    "-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key empty string",
			input:    "",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key underscore separator EXAMPLE_1",
			input:    "EXAMPLE_1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key with leading whitespace",
			input:    " EXAMPLE-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},
		{
			name:     "invalid key with trailing whitespace",
			input:    "EXAMPLE-1 ",
			site:     jiraSite,
			wantKind: classify.KindExternal,
		},

		// ── Jira issue URLs (KindJiraURL) ────────────────────────────────────

		{
			name:         "valid Jira browse URL",
			input:        "https://your-site.atlassian.net/browse/EXAMPLE-1",
			site:         jiraSite,
			wantKind:     classify.KindJiraURL,
			wantIssueKey: "EXAMPLE-1",
			wantURL:      "https://your-site.atlassian.net/browse/EXAMPLE-1",
		},
		{
			name:         "valid Jira browse URL with trailing slash",
			input:        "https://your-site.atlassian.net/browse/EXAMPLE-42/",
			site:         jiraSite,
			wantKind:     classify.KindJiraURL,
			wantIssueKey: "EXAMPLE-42",
			wantURL:      "https://your-site.atlassian.net/browse/EXAMPLE-42/",
		},
		{
			name:         "valid Jira browse URL with query string",
			input:        "https://your-site.atlassian.net/browse/EXAMPLE-7?focusedCommentId=123",
			site:         jiraSite,
			wantKind:     classify.KindJiraURL,
			wantIssueKey: "EXAMPLE-7",
			wantURL:      "https://your-site.atlassian.net/browse/EXAMPLE-7?focusedCommentId=123",
		},
		{
			name:         "Jira browse URL host case-insensitive",
			input:        "https://YOUR-SITE.ATLASSIAN.NET/browse/EXAMPLE-1",
			site:         jiraSite,
			wantKind:     classify.KindJiraURL,
			wantIssueKey: "EXAMPLE-1",
			wantURL:      "https://YOUR-SITE.ATLASSIAN.NET/browse/EXAMPLE-1",
		},

		// ── Jira URL with non-matching host → KindExternal ──────────────────

		{
			name:     "Jira-like URL wrong host",
			input:    "https://other-site.atlassian.net/browse/EXAMPLE-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://other-site.atlassian.net/browse/EXAMPLE-1",
		},
		{
			name:     "Jira URL empty jiraSite",
			input:    "https://your-site.atlassian.net/browse/EXAMPLE-1",
			site:     "",
			wantKind: classify.KindExternal,
			wantURL:  "https://your-site.atlassian.net/browse/EXAMPLE-1",
		},

		// ── Jira URL with non-browse path → KindExternal ────────────────────

		{
			name:     "Jira URL non-browse path /jira/EXAMPLE-1",
			input:    "https://your-site.atlassian.net/jira/EXAMPLE-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://your-site.atlassian.net/jira/EXAMPLE-1",
		},
		{
			name:     "Jira URL root path only",
			input:    "https://your-site.atlassian.net/",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://your-site.atlassian.net/",
		},
		{
			name:     "Jira URL browse path with invalid key",
			input:    "https://your-site.atlassian.net/browse/example-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://your-site.atlassian.net/browse/example-1",
		},

		// ── GitHub pull request URLs (KindGitHubPR) ─────────────────────────

		{
			name:         "valid GitHub PR URL",
			input:        "https://github.com/org/repo/pull/42",
			site:         jiraSite,
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 42,
			wantURL:      "https://github.com/org/repo/pull/42",
		},
		{
			name:         "GitHub PR URL with trailing slash",
			input:        "https://github.com/org/repo/pull/42/",
			site:         jiraSite,
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 42,
			wantURL:      "https://github.com/org/repo/pull/42/",
		},
		{
			name:         "GitHub PR URL with query string",
			input:        "https://github.com/org/repo/pull/42?diff=unified",
			site:         jiraSite,
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 42,
			wantURL:      "https://github.com/org/repo/pull/42?diff=unified",
		},
		{
			name:         "GitHub PR URL host case-insensitive",
			input:        "https://GITHUB.COM/org/repo/pull/99",
			site:         jiraSite,
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 99,
			wantURL:      "https://GITHUB.COM/org/repo/pull/99",
		},
		{
			name:         "GitHub PR URL large PR number",
			input:        "https://github.com/org/repo/pull/12345",
			site:         jiraSite,
			wantKind:     classify.KindGitHubPR,
			wantOwner:    "org",
			wantRepo:     "repo",
			wantPRNumber: 12345,
			wantURL:      "https://github.com/org/repo/pull/12345",
		},

		// ── GitHub URLs that are NOT pull requests → KindExternal ───────────

		{
			name:     "GitHub issues URL not a PR",
			input:    "https://github.com/org/repo/issues/42",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://github.com/org/repo/issues/42",
		},
		{
			name:     "GitHub tree URL not a PR",
			input:    "https://github.com/org/repo/tree/main",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://github.com/org/repo/tree/main",
		},
		{
			name:     "GitHub raw URL not a PR",
			input:    "https://raw.githubusercontent.com/org/repo/main/README.md",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://raw.githubusercontent.com/org/repo/main/README.md",
		},
		{
			name:     "GitHub repo root not a PR",
			input:    "https://github.com/org/repo",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://github.com/org/repo",
		},
		{
			name:     "GitHub pull URL with extra path segments not a PR",
			input:    "https://github.com/org/repo/pull/42/files",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://github.com/org/repo/pull/42/files",
		},
		{
			name:     "GitHub pull URL with non-numeric PR number",
			input:    "https://github.com/org/repo/pull/abc",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://github.com/org/repo/pull/abc",
		},

		// ── Generic external links (KindExternal) ───────────────────────────

		{
			name:     "external URL with path",
			input:    "https://example.com/doc",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://example.com/doc",
		},
		{
			name:     "external URL root",
			input:    "https://example.com",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "https://example.com",
		},
		{
			name:     "external HTTP URL",
			input:    "http://example.com/page",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "http://example.com/page",
		},
		{
			name:     "malformed URL no scheme",
			input:    "not-a-url",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "", // not parseable as a URL with scheme+host
		},
		{
			name:     "malformed URL just a path",
			input:    "/browse/EXAMPLE-1",
			site:     jiraSite,
			wantKind: classify.KindExternal,
			wantURL:  "", // no scheme/host
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classify.Classify(tt.input, tt.site)

			assert.Equal(t, tt.wantKind, got.Kind, "Kind")
			if tt.wantIssueKey != "" {
				assert.Equal(t, tt.wantIssueKey, got.IssueKey, "IssueKey")
			}
			if tt.wantURL != "" {
				assert.Equal(t, tt.wantURL, got.URL, "URL")
			}
			if tt.wantURL == "" && got.Kind == classify.KindExternal {
				// Only fail if we explicitly expect empty URL (wantURL == "" and
				// the test name signals "malformed" or "no scheme").
				// We use a sentinel: if wantURL is "" and wantKind is External,
				// check that URL is also "".
				assert.Empty(t, got.URL, "URL should be empty for non-URL input")
			}
			if tt.wantOwner != "" {
				assert.Equal(t, tt.wantOwner, got.Owner, "Owner")
			}
			if tt.wantRepo != "" {
				assert.Equal(t, tt.wantRepo, got.Repo, "Repo")
			}
			if tt.wantPRNumber != 0 {
				assert.Equal(t, tt.wantPRNumber, got.PRNumber, "PRNumber")
			}
		})
	}
}

// TestKindString verifies the String() method returns the expected labels.
func TestKindString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind classify.Kind
		want string
	}{
		{classify.KindJiraKey, "JiraKey"},
		{classify.KindJiraURL, "JiraURL"},
		{classify.KindGitHubPR, "GitHubPR"},
		{classify.KindExternal, "External"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.kind.String(), "Kind(%d).String()", c.kind)
	}
}
