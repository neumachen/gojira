// Package classify provides pure, stateless classification of strings into
// one of four link kinds: a bare Jira issue key, a Jira issue URL, a GitHub
// pull request URL, or an unclassified external link.
//
// It is a public leaf package: it imports nothing from the gojira module and
// has no I/O. Any package in the module (or any third-party Go program) may
// import it without pulling in crawl semantics, network code, or filesystem
// operations.
package classify

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind identifies the classification of a string passed to [Classify].
type Kind int

const (
	// KindJiraKey means the input is a bare Jira issue key such as "EXAMPLE-1".
	// The key matches [A-Z][A-Z0-9]+-[0-9]+ and is the whole input string.
	KindJiraKey Kind = iota

	// KindJiraURL means the input is a URL whose host matches the configured
	// Jira site and whose path is /browse/<KEY> for a valid issue key.
	KindJiraURL

	// KindGitHubPR means the input is a GitHub pull request URL of the form
	// https://github.com/<owner>/<repo>/pull/<N>.
	KindGitHubPR

	// KindExternal means the input did not match any of the above patterns.
	// It may be any other URL or an unrecognised string.
	KindExternal
)

// String returns a human-readable label for the Kind, useful for debugging
// and logging.
func (k Kind) String() string {
	switch k {
	case KindJiraKey:
		return "JiraKey"
	case KindJiraURL:
		return "JiraURL"
	case KindGitHubPR:
		return "GitHubPR"
	case KindExternal:
		return "External"
	default:
		return "Unknown"
	}
}

// Result holds the outcome of a [Classify] call.
//
// Fields that are not applicable to the returned Kind are left at their zero
// values (empty string / 0).
//
//   - KindJiraKey:  IssueKey is set.
//   - KindJiraURL:  IssueKey and URL are set.
//   - KindGitHubPR: Owner, Repo, PRNumber, and URL are set.
//   - KindExternal: URL is set when the input parses as a URL; otherwise URL
//     is empty.
type Result struct {
	Kind Kind

	// IssueKey is the Jira issue key (e.g. "EXAMPLE-1").
	// Set for KindJiraKey and KindJiraURL.
	IssueKey string

	// URL is the raw input string when it was recognised as a URL.
	// Set for KindJiraURL, KindGitHubPR, and KindExternal (when parseable).
	URL string

	// Owner is the GitHub repository owner.
	// Set for KindGitHubPR.
	Owner string

	// Repo is the GitHub repository name.
	// Set for KindGitHubPR.
	Repo string

	// PRNumber is the pull request number.
	// Set for KindGitHubPR.
	PRNumber int
}

// issueKeyRE matches a bare Jira issue key: one or more uppercase ASCII
// letters followed by one or more uppercase letters or digits, a hyphen, and
// one or more digits.  The anchors ensure the whole string must match.
var issueKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// Classify determines the kind of link represented by input.
//
// jiraSite is the base URL of the configured Jira Cloud site, for example
// "https://mycompany.atlassian.net". Only the host portion is used for
// matching; the scheme and path are ignored. An empty or unparseable jiraSite
// means no input will be classified as KindJiraURL.
//
// Classification rules, applied in order:
//  1. If input matches the bare issue-key pattern ([A-Z][A-Z0-9]+-[0-9]+,
//     whole string), return KindJiraKey.
//  2. If input parses as a URL whose host equals the jiraSite host
//     (case-insensitive) and whose path is /browse/<KEY> for a valid key,
//     return KindJiraURL.
//  3. If input parses as a URL with host "github.com" (case-insensitive) and
//     path /owner/repo/pull/N, return KindGitHubPR.
//  4. Otherwise return KindExternal. URL is filled when the input parses as a
//     URL; otherwise URL is empty.
func Classify(input string, jiraSite string) Result {
	// Rule 1: bare Jira issue key.
	if issueKeyRE.MatchString(input) {
		return Result{Kind: KindJiraKey, IssueKey: input}
	}

	// Parse the input as a URL for rules 2–4.
	u, err := url.Parse(input)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Not a URL at all → KindExternal with empty URL.
		return Result{Kind: KindExternal}
	}

	// Rule 2: Jira issue URL.
	if jiraSite != "" {
		if key, ok := matchJiraURL(u, jiraSite); ok {
			return Result{Kind: KindJiraURL, IssueKey: key, URL: input}
		}
	}

	// Rule 3: GitHub pull request URL.
	if owner, repo, prNum, ok := matchGitHubPR(u); ok {
		return Result{Kind: KindGitHubPR, Owner: owner, Repo: repo, PRNumber: prNum, URL: input}
	}

	// Rule 4: external link.
	return Result{Kind: KindExternal, URL: input}
}

// matchJiraURL returns the issue key and true when u is a Jira browse URL
// whose host matches the host extracted from jiraSite.
func matchJiraURL(u *url.URL, jiraSite string) (string, bool) {
	site, err := url.Parse(jiraSite)
	if err != nil || site.Host == "" {
		return "", false
	}
	if !strings.EqualFold(u.Host, site.Host) {
		return "", false
	}
	// Path must be exactly /browse/<KEY> (with optional trailing slash).
	path := strings.TrimSuffix(u.Path, "/")
	const prefix = "/browse/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	key := path[len(prefix):]
	if !issueKeyRE.MatchString(key) {
		return "", false
	}
	return key, true
}

// matchGitHubPR returns owner, repo, PR number, and true when u is a GitHub
// pull request URL: https://github.com/<owner>/<repo>/pull/<N>.
// Trailing slashes and query strings are tolerated; fragments are ignored.
func matchGitHubPR(u *url.URL) (owner, repo string, prNum int, ok bool) {
	if !strings.EqualFold(u.Host, "github.com") {
		return
	}
	// Split path into non-empty segments.
	path := strings.TrimSuffix(u.Path, "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// Expect exactly: <owner> <repo> "pull" <N>
	if len(parts) != 4 || !strings.EqualFold(parts[2], "pull") {
		return
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return
	}
	return parts[0], parts[1], n, true
}
