// Package parse converts raw Jira Cloud REST API v3 issue JSON into a typed
// Issue value. It is a pure package: no network calls, no filesystem access,
// and no project-internal imports. The caller supplies raw bytes (typically
// from internal/fetch) and a Jira site base URL; Parse returns a fully
// populated Issue or an error.
//
// Signature choice: Parse(raw []byte, site string) (Issue, error)
//
// The site parameter (e.g. "https://example.atlassian.net") is used to
// construct SourceURL as "<site>/browse/<KEY>". This keeps the Issue
// self-contained for downstream renderers without requiring them to know the
// site URL separately. If site is empty, SourceURL is left empty.
//
// Null description: a null or absent JSON description field is stored as a
// nil json.RawMessage (len == 0). Downstream consumers (internal/adf) must
// treat a nil/empty RawMessage as an empty document.
//
// Custom field detection: every key in the "fields" object whose name begins
// with "customfield_" and that does not have a dedicated typed slot in Issue
// is preserved verbatim in CustomFields as a json.RawMessage. No custom field
// is ever silently dropped (AC13).
package parse

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/neumachen/errext"
)

// Issue is the central typed representation of a Jira Cloud issue produced by
// Parse. It is consumed by internal/extract (link discovery), internal/render
// (Markdown generation), and internal/crawl (orchestration).
type Issue struct {
	// Key is the Jira issue key, e.g. "EXAMPLE-1".
	Key string

	// NumericID is the opaque integer ID Jira assigns to each issue,
	// distinct from the human-readable Key. It is the top-level "id"
	// field of the standard GET /issue/<KEY> response (a string of
	// digits, e.g. "86679"). gojira preserves it because the
	// undocumented Dev Status endpoint
	// (/rest/dev-status/1.0/issue/detail?issueId=<NUMERIC>) only
	// accepts the numeric form. Empty when the response omitted the
	// id field.
	NumericID string

	// Summary is the one-line issue title.
	Summary string

	// Status is the display name of the issue status, e.g. "In Progress".
	Status string

	// IssueType is the display name of the issue type, e.g. "Story".
	IssueType string

	// Assignee is the display name of the assignee, or empty if unassigned.
	Assignee string

	// Reporter is the display name of the reporter.
	Reporter string

	// Created is the issue creation timestamp.
	Created time.Time

	// Updated is the timestamp of the most recent issue update.
	Updated time.Time

	// SourceURL is the canonical Jira browse URL for this issue,
	// e.g. "https://example.atlassian.net/browse/EXAMPLE-1".
	// Empty when Parse is called with an empty site string.
	SourceURL string

	// Description holds the raw ADF JSON document from the issue's description
	// field. It is nil/empty when the description is null or absent. The adf
	// package is responsible for traversing this opaque value.
	Description json.RawMessage

	// Parent is the direct parent issue, or nil if the issue has no parent.
	Parent *ParentRef

	// Subtasks lists the direct child subtasks of this issue (i.e. legacy
	// Jira "Sub-task" type children carried inline on the parent issue's
	// fields.subtasks array).
	Subtasks []LinkedIssue

	// Children is the list of modern Jira hierarchy child keys discovered
	// for this issue via JQL search (parent = "KEY" and, where the Epic
	// Link custom field is configured/detected, "Epic Link" = "KEY").
	//
	// Parse() ALWAYS returns this as nil; it is populated externally by
	// internal/crawl after parsing (when children discovery is enabled)
	// so that the rendered output's Children section sits naturally
	// alongside Parent, Sub-tasks, and IssueLinks in the Relationships
	// group. Adding parsing logic for this field would be a layering
	// regression: hierarchy children are not encoded in the per-issue
	// GET response.
	Children []string

	// IssueLinks lists the explicit Jira issue links (blocks, relates to, etc.).
	IssueLinks []IssueLink

	// RemoteLinks lists the remote (external) links attached to this issue.
	RemoteLinks []RemoteLink

	// CustomFields preserves every "customfield_*" key from the Jira fields
	// object that does not have a dedicated typed slot above. Values are stored
	// as raw JSON so no information is lost regardless of the field type.
	// This map is never nil after a successful Parse call; it may be empty.
	CustomFields map[string]json.RawMessage

	// Names maps Jira field IDs (e.g. "customfield_10115") to their
	// human-readable labels (e.g. "Sprint"). Populated from the
	// top-level "names" object on the Jira issue response when the
	// caller requested expand=names; nil when absent.
	//
	// The map's keys are NOT guaranteed to be limited to custom
	// fields: standard field IDs ("summary", "status", "assignee",
	// ...) also appear in the names object. internal/render
	// consults this map only when it needs a label for a custom
	// field, and falls back to the raw customfield_NNNNN id when
	// the lookup misses. Entries with non-string values in the
	// response are skipped silently so a single malformed entry
	// cannot make the whole Names map disappear.
	Names map[string]string

	// DevStatus is the per-dataType bundle of development metadata
	// associated with this issue, discovered via Jira's Dev Status
	// endpoint by the crawl orchestrator. Parse() ALWAYS returns the
	// zero value; the inner slices are populated externally by
	// internal/crawl after parsing (when Dev Status enrichment is
	// enabled) so that the rendered "## Development" section sits
	// naturally between Description and Relationships. Adding parsing
	// logic here would be a layering regression: the per-issue GET
	// response only carries an opaque summary blob (customfield_10000)
	// with overall counts, not the actual entity lists.
	//
	// The five lists mirror the five dataType values the Jira UI
	// Development panel surfaces: pull requests, branches, commits,
	// repositories, and builds.
	DevStatus DevStatusData
}

// DevStatusData groups the five entity lists surfaced by Jira's Dev
// Status API: pull requests, branches, commits, repositories, and
// builds. Each list is independent and may be empty. The renderer
// elides the corresponding subsection when its list is empty.
//
// The type lives in parse so [Issue] can carry a single typed bundle
// and so internal/render can consume it without importing the
// devstatus package (which would create a cycle: devstatus imports
// parse for [Issue]). The crawl orchestrator owns the conversion from
// upstream client.DevStatus* shapes into these types via
// internal/devstatus.
type DevStatusData struct {
	PullRequests []PullRequest
	Branches     []Branch
	Commits      []Commit
	Repositories []Repository
	Builds       []Build
}

// PullRequest is the gojira-side representation of a pull request
// associated with a Jira issue, discovered via the Jira Dev Status API.
// Fields not provided by the upstream response are zero-valued.
//
// The type lives in parse so that internal/render can consume it
// without importing internal/devstatus (which would create a cycle:
// devstatus imports parse for [Issue]). The crawl orchestrator owns the
// conversion from the upstream client.DevStatusPR shape into this type
// via internal/devstatus.
type PullRequest struct {
	// ID is the upstream pull-request identifier as reported by Jira,
	// e.g. "#557" for GitHub or "1" for Bitbucket. The format varies
	// by provider; gojira preserves it verbatim.
	ID string

	// URL is the canonical web URL of the pull request, e.g.
	// "https://github.com/org/repo/pull/557". Used as the
	// deduplication key when merging results from multiple Dev Status
	// application queries.
	URL string

	// Title is the human-readable pull-request title (Jira's "name"
	// field on the upstream entry).
	Title string

	// Status is the upstream lifecycle state as reported by Jira:
	// "MERGED", "OPEN", "DECLINED", "DRAFT", or any other value the
	// provider returns. gojira does not normalise this.
	Status string

	// Application identifies the development-tool integration that
	// owns this pull request: "GitHub", "Bitbucket", "GitLab",
	// "GitHubEnterprise", etc. Populated by the devstatus enricher
	// from the configured applicationType used to query the entry.
	Application string

	// Repository is the upstream "repositoryName", e.g.
	// "org/repo". Empty when the provider does not surface a
	// repository name.
	Repository string

	// SourceBranch and DestBranch hold the source ("from") and
	// destination ("into") branch names. Either may be empty when
	// the upstream entry omits them.
	SourceBranch string
	DestBranch   string

	// Author is the pull-request author's display name (Jira's
	// "author.name"). Empty when omitted.
	Author string

	// LastUpdate is the parsed timestamp of the upstream "lastUpdate"
	// field. Zero when omitted or unparseable; preserving partial
	// data is preferable to dropping the entire entry.
	LastUpdate time.Time

	// Reviewers lists each reviewer's display name and approval
	// state. nil when the upstream entry omits reviewers entirely.
	Reviewers []Reviewer
}

// Reviewer holds a single pull-request reviewer entry.
type Reviewer struct {
	Name     string
	Approved bool
}

// Branch is the gojira-side representation of a development branch
// associated with a Jira issue, discovered via Jira's Dev Status API
// with dataType=branch. Fields not provided by the upstream response
// are zero-valued.
//
// LastCommit* fields capture the head commit of the branch as the
// upstream API reports it, so the renderer can surface a one-line
// summary without a separate commit lookup. LastCommitMsg is the first
// line of the upstream commit message (everything before the first
// newline); preserving multi-line commit bodies would bloat the
// Markdown listing.
type Branch struct {
	Name             string
	URL              string
	Repository       string // "org/repo"
	RepositoryURL    string
	LastCommitID     string // seven-character abbreviation (displayId)
	LastCommitURL    string
	LastCommitMsg    string // first line only
	LastCommitAuthor string
	LastUpdated      time.Time
}

// Commit is the gojira-side representation of a single development
// commit associated with a Jira issue, discovered via Jira's Dev Status
// API with dataType=commit. Fields not provided by the upstream
// response are zero-valued.
//
// ID is the full SHA; ShortID is the upstream displayId (typically
// seven characters) preserved verbatim rather than re-abbreviated
// locally. Message is the first line of the upstream commit message.
type Commit struct {
	ID         string
	ShortID    string
	URL        string
	Message    string // first line only
	Author     string
	Repository string // "org/repo"
	AuthoredAt time.Time
}

// Repository is the gojira-side representation of a single repository
// associated with a Jira issue, discovered via Jira's Dev Status API
// with dataType=repository. The Jira UI surfaces these so users can
// see which repositories reference an issue even when no PR or branch
// carries the issue key directly.
type Repository struct {
	Name string // "org/repo"
	URL  string
}

// Build is the gojira-side representation of a single CI build
// associated with a Jira issue, discovered via Jira's Dev Status API
// with dataType=build. State is the upstream lifecycle string
// ("SUCCESSFUL", "FAILED", "IN_PROGRESS", "STOPPED", "PENDING", ...);
// gojira does not normalise it.
//
// Tests* are extracted from the upstream testSummary block when
// present; all three fields are zero when the build did not publish a
// test summary, and the renderer elides the [tests P/T] suffix in that
// case.
type Build struct {
	ID          string
	Name        string
	URL         string
	State       string
	Description string
	Repository  string // "org/repo"; may be empty
	LastUpdated time.Time
	TestsPassed int
	TestsFailed int
	TestsTotal  int
}

// ParentRef holds the key and summary of a parent issue.
type ParentRef struct {
	Key     string
	Summary string
}

// LinkedIssue holds the key and summary of a subtask or similar lightweight
// issue reference.
type LinkedIssue struct {
	Key     string
	Summary string
}

// IssueLink represents a directed Jira issue link (e.g. "blocks", "relates to").
type IssueLink struct {
	// Direction is "inward" or "outward", indicating which side of the link
	// relationship this issue occupies.
	Direction string

	// Type is the link type name as returned by Jira, e.g. "Blocks" or "Relates".
	Type string

	// Key is the issue key of the linked issue.
	Key string

	// Summary is the summary of the linked issue.
	Summary string
}

// RemoteLink represents a remote (external) link attached to a Jira issue.
type RemoteLink struct {
	Title string
	URL   string
}

// jiraTime is a helper for parsing Jira's ISO-8601 timestamps, which may
// include milliseconds (e.g. "2026-01-15T10:30:00.000+0000") or omit them
// (e.g. "2026-01-15T10:30:00+0000"). Both formats are tried in order.
var jiraTimeLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339Nano,
	time.RFC3339,
}

func parseJiraTime(s string) (time.Time, error) {
	for _, layout := range jiraTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errext.Errorf("parse: unrecognised time format %q", s)
}

// --- internal JSON shapes for unmarshalling ---

// These unexported types mirror the Jira Cloud REST API v3 response shape.
// They are used only inside Parse and are not part of the package API.

type rawIssue struct {
	ID     string          `json:"id"`
	Key    string          `json:"key"`
	Self   string          `json:"self"`
	Fields json.RawMessage `json:"fields"`
	// Names is the top-level "names" object the Jira API surfaces
	// when the request was issued with expand=names. It is captured
	// as raw JSON so we can unmarshal it into a permissive
	// map[string]json.RawMessage and skip non-string entries
	// individually rather than failing the whole field set when one
	// entry has an unexpected shape.
	Names json.RawMessage `json:"names"`
}

type rawUser struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

func (u *rawUser) displayName() string {
	if u == nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.EmailAddress
}

type rawStatus struct {
	Name string `json:"name"`
}

type rawIssueType struct {
	Name string `json:"name"`
}

type rawParent struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type rawSubtask struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type rawIssueLinkType struct {
	Name    string `json:"name"`
	Inward  string `json:"inward"`
	Outward string `json:"outward"`
}

type rawLinkedIssue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type rawIssueLink struct {
	Type         rawIssueLinkType `json:"type"`
	InwardIssue  *rawLinkedIssue  `json:"inwardIssue"`
	OutwardIssue *rawLinkedIssue  `json:"outwardIssue"`
}

type rawRemoteLinkObject struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type rawRemoteLink struct {
	Object rawRemoteLinkObject `json:"object"`
}

// knownFields is the set of field names in the Jira "fields" object that have
// dedicated typed slots in Issue. Every other "customfield_*" key is preserved
// in CustomFields.
var knownFields = map[string]bool{
	"summary":     true,
	"status":      true,
	"issuetype":   true,
	"assignee":    true,
	"reporter":    true,
	"created":     true,
	"updated":     true,
	"description": true,
	"parent":      true,
	"subtasks":    true,
	"issuelinks":  true,
	"remotelinks": true,
}

// Parse converts raw Jira Cloud REST API v3 issue JSON into a typed Issue.
//
// site is the Jira Cloud base URL (e.g. "https://example.atlassian.net") used
// to construct SourceURL. Pass an empty string to leave SourceURL empty.
//
// All "customfield_*" keys in the fields object that do not have a dedicated
// typed slot are preserved verbatim in Issue.CustomFields (never dropped).
func Parse(raw []byte, site string) (Issue, error) {
	var ri rawIssue
	if err := json.Unmarshal(raw, &ri); err != nil {
		return Issue{}, errext.Errorf("parse: unmarshal issue: %w", err)
	}
	if ri.Key == "" {
		return Issue{}, errext.Errorf("parse: issue JSON missing required field \"key\"")
	}

	// Unmarshal the fields object into a generic map so we can iterate over
	// all keys, including unknown customfield_* entries.
	var allFields map[string]json.RawMessage
	if err := json.Unmarshal(ri.Fields, &allFields); err != nil {
		return Issue{}, errext.Errorf("parse: unmarshal fields for %s: %w", ri.Key, err)
	}

	issue := Issue{
		Key:          ri.Key,
		NumericID:    ri.ID,
		CustomFields: make(map[string]json.RawMessage),
	}

	// Build SourceURL from the site parameter.
	if site != "" {
		issue.SourceURL = strings.TrimRight(site, "/") + "/browse/" + ri.Key
	}

	// Populate Names from the top-level "names" object, if the
	// caller requested expand=names and the response carried it.
	// Errors are non-fatal: a malformed names object leaves
	// issue.Names nil and the renderer falls back to the raw field
	// ID. Individual entries whose value is not a JSON string are
	// skipped, preserving the well-formed entries around them.
	if len(ri.Names) > 0 && string(ri.Names) != "null" {
		var rawNames map[string]json.RawMessage
		if err := json.Unmarshal(ri.Names, &rawNames); err == nil {
			names := make(map[string]string, len(rawNames))
			for k, v := range rawNames {
				// Skip JSON null explicitly. json.Unmarshal happily
				// decodes the `null` literal into a "" string value;
				// that would let null entries (which carry no label)
				// be confused with intentional empty-string labels.
				if isJSONNull(v) {
					continue
				}
				var s string
				if err := json.Unmarshal(v, &s); err == nil {
					names[k] = s
				}
				// Non-string, non-null entries are skipped: the
				// names map is best-effort enrichment, not a
				// load-bearing invariant.
			}
			if len(names) > 0 {
				issue.Names = names
			}
		}
	}

	// summary
	if v, ok := allFields["summary"]; ok {
		if err := json.Unmarshal(v, &issue.Summary); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal summary for %s: %w", ri.Key, err)
		}
	}

	// status
	if v, ok := allFields["status"]; ok {
		var s rawStatus
		if err := json.Unmarshal(v, &s); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal status for %s: %w", ri.Key, err)
		}
		issue.Status = s.Name
	}

	// issuetype
	if v, ok := allFields["issuetype"]; ok {
		var it rawIssueType
		if err := json.Unmarshal(v, &it); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal issuetype for %s: %w", ri.Key, err)
		}
		issue.IssueType = it.Name
	}

	// assignee (may be null)
	if v, ok := allFields["assignee"]; ok && string(v) != "null" {
		var u rawUser
		if err := json.Unmarshal(v, &u); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal assignee for %s: %w", ri.Key, err)
		}
		issue.Assignee = u.displayName()
	}

	// reporter
	if v, ok := allFields["reporter"]; ok && string(v) != "null" {
		var u rawUser
		if err := json.Unmarshal(v, &u); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal reporter for %s: %w", ri.Key, err)
		}
		issue.Reporter = u.displayName()
	}

	// created
	if v, ok := allFields["created"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal created for %s: %w", ri.Key, err)
		}
		t, err := parseJiraTime(s)
		if err != nil {
			return Issue{}, errext.Errorf("parse: created for %s: %w", ri.Key, err)
		}
		issue.Created = t
	}

	// updated
	if v, ok := allFields["updated"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal updated for %s: %w", ri.Key, err)
		}
		t, err := parseJiraTime(s)
		if err != nil {
			return Issue{}, errext.Errorf("parse: updated for %s: %w", ri.Key, err)
		}
		issue.Updated = t
	}

	// description (ADF document or null)
	if v, ok := allFields["description"]; ok && string(v) != "null" {
		issue.Description = v
	}
	// If absent or null, Description remains nil — documented behaviour.

	// parent (may be null or absent)
	if v, ok := allFields["parent"]; ok && string(v) != "null" {
		var p rawParent
		if err := json.Unmarshal(v, &p); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal parent for %s: %w", ri.Key, err)
		}
		if p.Key != "" {
			issue.Parent = &ParentRef{Key: p.Key, Summary: p.Fields.Summary}
		}
	}

	// subtasks
	if v, ok := allFields["subtasks"]; ok && string(v) != "null" {
		var subs []rawSubtask
		if err := json.Unmarshal(v, &subs); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal subtasks for %s: %w", ri.Key, err)
		}
		for _, s := range subs {
			issue.Subtasks = append(issue.Subtasks, LinkedIssue{Key: s.Key, Summary: s.Fields.Summary})
		}
	}

	// issuelinks
	if v, ok := allFields["issuelinks"]; ok && string(v) != "null" {
		var links []rawIssueLink
		if err := json.Unmarshal(v, &links); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal issuelinks for %s: %w", ri.Key, err)
		}
		for _, l := range links {
			if l.OutwardIssue != nil {
				issue.IssueLinks = append(issue.IssueLinks, IssueLink{
					Direction: "outward",
					Type:      l.Type.Name,
					Key:       l.OutwardIssue.Key,
					Summary:   l.OutwardIssue.Fields.Summary,
				})
			}
			if l.InwardIssue != nil {
				issue.IssueLinks = append(issue.IssueLinks, IssueLink{
					Direction: "inward",
					Type:      l.Type.Name,
					Key:       l.InwardIssue.Key,
					Summary:   l.InwardIssue.Fields.Summary,
				})
			}
		}
	}

	// remotelinks — note: the Jira REST API returns remote links at a separate
	// endpoint (/rest/api/3/issue/{key}/remotelink), but some callers embed
	// them in the fields object for convenience. We handle both: if the field
	// is present here, we parse it; if absent, RemoteLinks stays nil.
	if v, ok := allFields["remotelinks"]; ok && string(v) != "null" {
		var rls []rawRemoteLink
		if err := json.Unmarshal(v, &rls); err != nil {
			return Issue{}, errext.Errorf("parse: unmarshal remotelinks for %s: %w", ri.Key, err)
		}
		for _, rl := range rls {
			if rl.Object.URL != "" {
				issue.RemoteLinks = append(issue.RemoteLinks, RemoteLink{
					Title: rl.Object.Title,
					URL:   rl.Object.URL,
				})
			}
		}
	}

	// Preserve all customfield_* keys not already handled above.
	for k, v := range allFields {
		if !strings.HasPrefix(k, "customfield_") {
			continue
		}
		if knownFields[k] {
			continue
		}
		issue.CustomFields[k] = v
	}

	return issue, nil
}

// isJSONNull reports whether v is the four-byte JSON null literal
// (possibly surrounded by ASCII whitespace). Used by the names
// parser to distinguish a null entry — which carries no label —
// from an intentional empty-string label, both of which
// json.Unmarshal would otherwise decode into "".
func isJSONNull(v json.RawMessage) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		// First non-whitespace byte: it's null only if the prefix
		// matches "null" exactly and nothing follows it but
		// whitespace.
		if i+4 > len(v) || string(v[i:i+4]) != "null" {
			return false
		}
		for j := i + 4; j < len(v); j++ {
			c := v[j]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				return false
			}
		}
		return true
	}
	return false
}
