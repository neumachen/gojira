// Package render converts a parsed Jira issue into Markdown content.
//
// # Design boundary
//
// render is a pure package: no network calls, no filesystem access. It
// accepts a parse.Issue value and produces Markdown strings. The caller
// (internal/crawl or the gojira facade) is responsible for writing those
// strings to disk via internal/output.
//
// render imports only internal/parse and internal/adf. It deliberately does
// NOT import classify. Link classification is the caller's responsibility:
// the caller must pre-classify outbound references and pass them in as
// []OutboundRef. The mapping from classify.Kind constants to the string
// labels used by OutboundRef.Kind ("jira", "github-pr", "external") is the
// caller's responsibility and must be documented at the call site.
//
// # Neighbour resolution
//
// RenderIssue accepts a neighbours map (issue key → true) representing the
// set of issue keys that have already been downloaded. When a relationship
// link target key is present in neighbours, the rendered link is a relative
// Markdown path (../KEY/index.md). When the key is absent from neighbours
// (unresolved, permission-denied, or cap-limited), the rendered link falls
// back to an absolute Jira browse URL derived from the issue's own SourceURL
// base. If SourceURL is empty, the key is rendered as plain text with no
// hyperlink.
//
// # Unknown ADF node visibility
//
// When adf.RenderMarkdown encounters an unknown node type it emits an inline
// Markdown comment (<!-- adf: unknown node type "..." -->) and preserves any
// inner text. RenderIssue additionally appends an "## Unknown content"
// section listing each unknown node type. This maximises visibility of
// preserved-but-unrendered content in v0.1 and can be removed once the ADF
// renderer covers all node types in use.
//
// # Section elision
//
// Sections with no content are omitted entirely from the output. The
// Relationships heading is omitted when all subsections (Parent, Sub-tasks,
// Children, Linked issues, Remote links) are empty. The Custom fields
// section is omitted when issue.CustomFields is empty. The Unknown content
// section is omitted when adf.RenderMarkdown returns no unknown nodes.
//
// # Sub-tasks vs. Children
//
// Two relationship subsections render Jira's two distinct child concepts:
//
//   - "### Sub-tasks" lists issue.Subtasks, the legacy Jira "Sub-task" type
//     children returned inline on the parent's GET response.
//   - "### Children" lists issue.Children, the modern hierarchy children
//     discovered by internal/crawl via JQL search (parent = "KEY" and,
//     where the Epic Link field exists, "Epic Link" = "KEY") after the
//     issue has been parsed.
//
// Earlier versions of gojira rendered issue.Subtasks under "### Children".
// The rename clarifies the distinction and matches Jira's own vocabulary;
// it is documented in the package change log of the design mini-doc (v4).
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/adf"
	"github.com/neumachen/gojira/internal/parse"
)

// OutboundRef is a pre-classified outbound reference passed to RenderOutbound
// by the caller. render does not import classify; the caller is responsible
// for mapping classify.Kind constants to the Kind string labels below.
//
// Kind values:
//   - "jira"      — a Jira issue reference (IssueKey is set)
//   - "github-pr" — a GitHub pull request (Owner, Repo, PRNumber are set)
//   - "external"  — any other URL (URL and Text are set)
type OutboundRef struct {
	// Kind is one of "jira", "github-pr", or "external".
	Kind string

	// IssueKey is the Jira issue key for Kind=="jira" references.
	IssueKey string

	// URL is the raw URL for "github-pr" and "external" references.
	URL string

	// Text is the display label for "external" references. If empty, URL is
	// used as the label.
	Text string

	// Owner, Repo, PRNumber are set for Kind=="github-pr" references.
	Owner    string
	Repo     string
	PRNumber int
}

// RenderIssue produces the Markdown content for <KEY>/index.md.
//
// neighbours is the set of already-downloaded issue keys. A key present in
// neighbours gets a relative link (../KEY/index.md); an absent key gets an
// absolute Jira browse URL (derived from issue.SourceURL) or plain text if
// SourceURL is empty.
//
// renderNullCustomFields toggles the behaviour of the "## Custom
// fields" section when a custom field's JSON value is `null`:
//
//   - false (default): null-valued custom fields are skipped entirely.
//     On a typical Jira tenant the vast majority of customfield_*
//     entries are null per issue, so suppressing them removes the
//     noise without losing information.
//   - true: each null-valued custom field renders as
//     `- <label>: null`, preserving every field's presence for
//     audits.
//
// The flag is a positional bool rather than an options struct because
// it is the only renderer toggle today; the project's signature-
// honesty rule (docs/engineering-principles.md) prefers an explicit
// parameter to a hidden constant. Add an options struct if a third
// toggle ever lands.
//
// Sections with no content are omitted. The Relationships heading is omitted
// when all subsections are empty. The Custom fields section is omitted when
// every entry would be elided (default config + all-null fields) or when
// issue.CustomFields is empty. The Unknown content section is omitted when
// the ADF renderer encounters no unknown nodes.
func RenderIssue(issue parse.Issue, neighbours map[string]bool, renderNullCustomFields bool) (string, error) {
	var sb strings.Builder

	// Derive the Jira site base URL from issue.SourceURL for unresolved links.
	siteBase := extractSiteBase(issue.SourceURL)

	// --- Heading ---
	sb.WriteString("# ")
	sb.WriteString(issue.Key)
	if issue.Summary != "" {
		sb.WriteString(" — ")
		sb.WriteString(issue.Summary)
	}
	sb.WriteString("\n")

	// --- Metadata ---
	sb.WriteString("\n## Metadata\n\n")
	assignee := issue.Assignee
	if assignee == "" {
		assignee = "Unassigned"
	}
	sb.WriteString(fmt.Sprintf("- Status: %s\n", issue.Status))
	sb.WriteString(fmt.Sprintf("- Type: %s\n", issue.IssueType))
	sb.WriteString(fmt.Sprintf("- Assignee: %s\n", assignee))
	sb.WriteString(fmt.Sprintf("- Reporter: %s\n", issue.Reporter))
	if !issue.Created.IsZero() {
		sb.WriteString(fmt.Sprintf("- Created: %s\n", issue.Created.UTC().Format("2006-01-02T15:04:05Z")))
	}
	if !issue.Updated.IsZero() {
		sb.WriteString(fmt.Sprintf("- Updated: %s\n", issue.Updated.UTC().Format("2006-01-02T15:04:05Z")))
	}
	if issue.SourceURL != "" {
		sb.WriteString(fmt.Sprintf("- Source: [%s](%s)\n", issue.Key, issue.SourceURL))
	}

	// --- Description ---
	descMD, unknownNodes, err := adf.RenderMarkdown(issue.Description)
	if err != nil {
		return "", errext.Errorf("render: ADF render for %s: %w", issue.Key, err)
	}
	sb.WriteString("\n## Description\n")
	if strings.TrimSpace(descMD) == "" {
		sb.WriteString("\n*No description.*\n")
	} else {
		sb.WriteString("\n")
		sb.WriteString(descMD)
		// Ensure the description ends with a newline.
		if !strings.HasSuffix(descMD, "\n") {
			sb.WriteString("\n")
		}
	}

	// --- Development ---
	//
	// Emit a "## Development" section listing every kind of development
	// entity Jira's Dev Status API surfaces for this issue (PRs,
	// branches, commits, repositories, builds). The section is
	// populated externally by internal/crawl from internal/devstatus
	// via issue.DevStatus; parse never sets it.
	//
	// The section sits between Description and Relationships so it is
	// visible alongside the issue body without polluting the
	// Relationships group (which is conceptually about Jira-internal
	// links). Five subsections are emitted in the canonical Jira UI
	// Development panel order:
	//
	//   ### Pull requests   (issue.DevStatus.PullRequests)
	//   ### Branches        (issue.DevStatus.Branches)
	//   ### Commits         (issue.DevStatus.Commits)
	//   ### Repositories    (issue.DevStatus.Repositories)
	//   ### Builds          (issue.DevStatus.Builds)
	//
	// Each subsection is independently elided when its list is empty.
	// The parent "## Development" header is elided when ALL five lists
	// are empty.
	dev := issue.DevStatus
	hasPRs := len(dev.PullRequests) > 0
	hasBranches := len(dev.Branches) > 0
	hasCommits := len(dev.Commits) > 0
	hasRepos := len(dev.Repositories) > 0
	hasBuilds := len(dev.Builds) > 0
	if hasPRs || hasBranches || hasCommits || hasRepos || hasBuilds {
		sb.WriteString("\n## Development\n")
		if hasPRs {
			sb.WriteString("\n### Pull requests\n\n")
			for _, pr := range dev.PullRequests {
				sb.WriteString(renderPullRequest(pr))
			}
		}
		if hasBranches {
			sb.WriteString("\n### Branches\n\n")
			for _, br := range dev.Branches {
				sb.WriteString(renderBranch(br))
			}
		}
		if hasCommits {
			sb.WriteString("\n### Commits\n\n")
			for _, cm := range dev.Commits {
				sb.WriteString(renderCommit(cm))
			}
		}
		if hasRepos {
			sb.WriteString("\n### Repositories\n\n")
			for _, rp := range dev.Repositories {
				sb.WriteString(renderRepository(rp))
			}
		}
		if hasBuilds {
			sb.WriteString("\n### Builds\n\n")
			for _, bd := range dev.Builds {
				sb.WriteString(renderBuild(bd))
			}
		}
	}

	// --- Relationships ---
	hasParent := issue.Parent != nil
	hasSubtasks := len(issue.Subtasks) > 0
	hasChildren := len(issue.Children) > 0
	hasLinked := len(issue.IssueLinks) > 0
	hasRemote := len(issue.RemoteLinks) > 0

	if hasParent || hasSubtasks || hasChildren || hasLinked || hasRemote {
		sb.WriteString("\n## Relationships\n")

		if hasParent {
			sb.WriteString("\n### Parent\n\n")
			sb.WriteString("- ")
			sb.WriteString(issueLink(issue.Parent.Key, siteBase, neighbours))
			sb.WriteString("\n")
		}

		if hasSubtasks {
			sb.WriteString("\n### Sub-tasks\n\n")
			for _, sub := range issue.Subtasks {
				sb.WriteString("- ")
				sb.WriteString(issueLink(sub.Key, siteBase, neighbours))
				sb.WriteString("\n")
			}
		}

		if hasChildren {
			sb.WriteString("\n### Children\n\n")
			for _, ck := range issue.Children {
				sb.WriteString("- ")
				sb.WriteString(issueLink(ck, siteBase, neighbours))
				sb.WriteString("\n")
			}
		}

		if hasLinked {
			sb.WriteString("\n### Linked issues\n\n")
			for _, link := range issue.IssueLinks {
				relation := issueLinkRelation(link)
				sb.WriteString(fmt.Sprintf("- %s %s\n", relation, issueLink(link.Key, siteBase, neighbours)))
			}
		}

		if hasRemote {
			sb.WriteString("\n### Remote links\n\n")
			for _, rl := range issue.RemoteLinks {
				label := rl.Title
				if label == "" {
					label = rl.URL
				}
				sb.WriteString(fmt.Sprintf("- [%s](%s)\n", label, rl.URL))
			}
		}
	}

	// --- Custom fields ---
	//
	// Each entry classifies into one of five kinds (see
	// classifyCustomField): null, primitive, structured, invalid,
	// string-structured.
	//   - null entries are skipped unless renderNullCustomFields is true.
	//   - primitive entries (string, number, bool) render inline:
	//       "- <label>: <value>"
	//   - structured entries (JSON object/array, or a JSON string
	//     whose decoded contents are themselves a JSON object/array)
	//     render as a fenced ```json code block, pretty-printed via
	//     json.Indent with a two-space indent.
	//   - invalid entries (bytes that are not valid JSON at the
	//     outer layer) render in a plain ``` block with no language
	//     tag — honest about what we know. This is rare in practice
	//     since Jira's REST API speaks JSON; the kind is preserved
	//     as a separate signal from string-structured so future
	//     divergence (e.g. different debug output, different ACs)
	//     stays cheap.
	//   - string-structured entries (a JSON string whose decoded
	//     contents are non-JSON but contain at least one of `{`,
	//     `[`, `=`, or '\n' — the canonical example is Atlassian's
	//     customfield_10000 "Dev Status summary" blob in its
	//     `{repository={count=1, ...}, json={...}}` mixed notation)
	//     render in a fenced ```json block with the inner content
	//     verbatim and the outer JSON-string quotes stripped. The
	//     content is not strictly valid JSON but is dominated by
	//     JSON-shaped tokens (quoted strings, numbers, colon-and-
	//     comma punctuation), and the json tag is a syntax-
	//     highlighting hint that Markdown viewers do not validate.
	//     Tagging consistently with the structured case keeps the
	//     visual story uniform — readers see one Markdown shape
	//     for "field with structured content," not two. The format
	//     matches the
	//     invalid case today; the two kinds stay separate because
	//     they have semantically different origins.
	//
	// IMPORTANT: a fenced code block nested under a Markdown list
	// bullet must be indented to the bullet's content column (two
	// spaces) for renderers like GitHub/GitLab/Obsidian to treat it
	// as nested. Without that indentation the fence terminates the
	// list. indentByTwo handles the per-line prefixing.
	//
	// Keys are sorted lexicographically for deterministic output;
	// the names map is consulted for the label but does NOT
	// influence sort order (labels can change; IDs do not).
	if len(issue.CustomFields) > 0 {
		ids := make([]string, 0, len(issue.CustomFields))
		for k := range issue.CustomFields {
			ids = append(ids, k)
		}
		sort.Strings(ids)

		var fieldLines []string
		for _, id := range ids {
			raw := issue.CustomFields[id]
			kind, pretty, indented := classifyCustomField(raw)
			if kind == kindNull && !renderNullCustomFields {
				continue
			}
			label := labelFor(id, issue.Names)
			switch kind {
			case kindPrimitive, kindNull:
				fieldLines = append(fieldLines,
					fmt.Sprintf("- %s: %s", label, pretty))
			case kindStructured:
				if indented {
					fieldLines = append(fieldLines,
						fmt.Sprintf("- %s:\n  ```json\n%s\n  ```",
							label, indentByTwo(pretty)))
				} else {
					// Single-line structured values (e.g. "[]" or "{}").
					fieldLines = append(fieldLines,
						fmt.Sprintf("- %s: %s", label, pretty))
				}
			case kindStringStructured:
				// JSON string whose decoded content is structured
				// text. Renders in a fenced ```json block because
				// the content is dominated by JSON-shaped tokens
				// (quoted strings, numbers, colon/comma
				// punctuation), and the json tag is a syntax-
				// highlighting hint that no Markdown renderer
				// validates. Tagging consistently with the
				// structured case keeps the visual story uniform.
				//
				// The content arrives as a single line of mixed
				// Java Map.toString()+JSON notation (Atlassian's
				// customfield_10000 Dev Status summary blob is
				// the canonical example). prettifyAtlassianBlob
				// walks the bytes, tracks brace depth, inserts
				// newlines and indents, and delegates embedded
				// json={...} payloads to json.Indent — yielding
				// a multi-line indented render that is dramatically
				// more legible than the one-line verbatim form.
				//
				// The walker is conservative: any structural
				// surprise (unbalanced braces, an unparseable
				// json= payload) yields ok=false and the renderer
				// falls back to emitting the content verbatim
				// inside the same ```json fence. Partial-mangled
				// output is never returned.
				body := pretty
				if prettyMultiline, ok := prettifyAtlassianBlob(pretty); ok {
					body = prettyMultiline
				}
				fieldLines = append(fieldLines,
					fmt.Sprintf("- %s:\n  ```json\n%s\n  ```",
						label, indentByTwo(body)))
			case kindInvalid:
				// Raw bytes are not valid JSON at all. Render in
				// a plain ``` fence with no language tag —
				// honest about what we know. This branch is rare
				// in practice since Jira's REST API speaks JSON;
				// it exists as a defensive default.
				fieldLines = append(fieldLines,
					fmt.Sprintf("- %s:\n  ```\n%s\n  ```",
						label, indentByTwo(pretty)))
			}
		}

		if len(fieldLines) > 0 {
			sb.WriteString("\n## Custom fields\n\n")
			for _, line := range fieldLines {
				sb.WriteString(line)
				sb.WriteString("\n")
			}
		}
	}

	// --- Unknown content ---
	// Emit an "## Unknown content" section listing every unknown ADF node type
	// encountered during description rendering. This maximises visibility of
	// preserved-but-unrendered content in v0.1 (see package doc).
	if len(unknownNodes) > 0 {
		sb.WriteString("\n## Unknown content\n\n")
		// Deduplicate node types while preserving first-seen order.
		seen := make(map[string]bool)
		for _, un := range unknownNodes {
			if !seen[un.NodeType] {
				seen[un.NodeType] = true
				sb.WriteString(fmt.Sprintf("- Unknown ADF node type: `%s` (content preserved inline above)\n", un.NodeType))
			}
		}
	}

	return sb.String(), nil
}

// RenderOutbound produces the Markdown content for <KEY>/references/outbound.md.
//
// refs is a slice of pre-classified outbound references. The caller is
// responsible for populating refs with the correct Kind labels.
//
// Sections with no content are omitted. If refs is empty, an empty string is
// returned with no error — the caller (internal/output) treats an empty string
// as "do not create outbound.md".
func RenderOutbound(refs []OutboundRef) (string, error) {
	if len(refs) == 0 {
		return "", nil
	}

	var jiraRefs, prRefs, extRefs []OutboundRef
	for _, r := range refs {
		switch r.Kind {
		case "jira":
			jiraRefs = append(jiraRefs, r)
		case "github-pr":
			prRefs = append(prRefs, r)
		default:
			extRefs = append(extRefs, r)
		}
	}

	// If all refs are of unknown kind, treat them as external.
	if len(jiraRefs) == 0 && len(prRefs) == 0 && len(extRefs) == 0 {
		return "", nil
	}

	var sb strings.Builder
	sb.WriteString("# Outbound references\n")

	if len(jiraRefs) > 0 {
		sb.WriteString("\n## Jira issues\n\n")
		for _, r := range jiraRefs {
			if r.IssueKey != "" {
				// Relative link: the outbound.md lives at <KEY>/references/outbound.md,
				// so a sibling issue is at ../../OTHER-KEY/index.md.
				sb.WriteString(fmt.Sprintf("- [%s](../../%s/index.md)\n", r.IssueKey, r.IssueKey))
			} else if r.URL != "" {
				label := r.Text
				if label == "" {
					label = r.URL
				}
				sb.WriteString(fmt.Sprintf("- [%s](%s)\n", label, r.URL))
			}
		}
	}

	if len(prRefs) > 0 {
		sb.WriteString("\n## Pull requests\n\n")
		for _, r := range prRefs {
			label := fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.PRNumber)
			sb.WriteString(fmt.Sprintf("- [%s](%s)\n", label, r.URL))
		}
	}

	if len(extRefs) > 0 {
		sb.WriteString("\n## External\n\n")
		for _, r := range extRefs {
			label := r.Text
			if label == "" {
				label = r.URL
			}
			sb.WriteString(fmt.Sprintf("- [%s](%s)\n", label, r.URL))
		}
	}

	return sb.String(), nil
}

// RenderStub produces the Markdown content for <KEY>/index.md when the issue
// could not be fetched (e.g. 403 Permission denied or 404 Not found).
//
// The stub format is:
//
//	# KEY
//
//	> Could not fetch: <reason>
//
//	- Source: [KEY](<jiraBrowseURL>)  (omitted when sourceURL is empty)
//
// reason is a human-readable string supplied by the caller, e.g.
// "Permission denied (403)" or "Not found (404)".
func RenderStub(key string, reason string, sourceURL string) (string, error) {
	var sb strings.Builder
	sb.WriteString("# ")
	sb.WriteString(key)
	sb.WriteString("\n\n")
	sb.WriteString("> Could not fetch: ")
	sb.WriteString(reason)
	sb.WriteString("\n")
	if sourceURL != "" {
		sb.WriteString("\n")
		sb.WriteString(fmt.Sprintf("- Source: [%s](%s)\n", key, sourceURL))
	}
	return sb.String(), nil
}

// ---- helpers ---------------------------------------------------------------

// issueLink returns a Markdown link for a related issue key.
// If the key is in neighbours (already downloaded), a relative path is used.
// Otherwise an absolute Jira browse URL is used (if siteBase is known) or
// the key is rendered as plain text.
func issueLink(key, siteBase string, neighbours map[string]bool) string {
	if neighbours[key] {
		return fmt.Sprintf("[%s](../%s/index.md)", key, key)
	}
	if siteBase != "" {
		return fmt.Sprintf("[%s](%s/browse/%s)", key, siteBase, key)
	}
	return key
}

// issueLinkRelation returns the human-readable relation label for an IssueLink.
// For outward links the outward type name is used (e.g. "blocks").
// For inward links the inward type name is used (e.g. "is blocked by").
// The Type field from parse holds the link type name (e.g. "Blocks"); we
// lower-case it for readability.
func issueLinkRelation(link parse.IssueLink) string {
	return strings.ToLower(link.Type)
}

// extractSiteBase derives the Jira site base URL (e.g.
// "https://example.atlassian.net") from a full browse URL like
// "https://example.atlassian.net/browse/EXAMPLE-1". Returns "" if the URL
// cannot be parsed or has no recognisable /browse/ segment.
func extractSiteBase(sourceURL string) string {
	if sourceURL == "" {
		return ""
	}
	u, err := url.Parse(sourceURL)
	if err != nil {
		return ""
	}
	// Strip everything from /browse/ onward.
	idx := strings.Index(u.Path, "/browse/")
	if idx < 0 {
		// No /browse/ segment; return scheme+host as the base.
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + "://" + u.Host + u.Path[:idx]
}

// renderBranch formats a single [parse.Branch] as one Markdown bullet
// line ending with a newline.
//
// Format:
//
//   - [<name>](<url>) — `<repository>` · last: <short-commit-id>
//
// The short-commit-id is wrapped in a Markdown link to LastCommitURL
// when present, so a click reaches the head commit directly. The
// repository segment and the "last: ..." segment are independently
// elided when their underlying fields are empty so branches from
// providers that omit metadata still render cleanly.
func renderBranch(br parse.Branch) string {
	var sb strings.Builder

	name := br.Name
	if name == "" {
		name = br.URL
	}

	sb.WriteString("- ")
	if br.URL != "" {
		sb.WriteString("[")
		sb.WriteString(name)
		sb.WriteString("](")
		sb.WriteString(br.URL)
		sb.WriteString(")")
	} else {
		sb.WriteString(name)
	}
	if br.Repository != "" {
		sb.WriteString(" — `")
		sb.WriteString(br.Repository)
		sb.WriteString("`")
	}
	if br.LastCommitID != "" {
		sb.WriteString(" · last: ")
		if br.LastCommitURL != "" {
			sb.WriteString("[")
			sb.WriteString(br.LastCommitID)
			sb.WriteString("](")
			sb.WriteString(br.LastCommitURL)
			sb.WriteString(")")
		} else {
			sb.WriteString(br.LastCommitID)
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderCommit formats a single [parse.Commit] as one Markdown bullet
// line ending with a newline.
//
// Format:
//
//   - [<short-id>](<url>) — "<message-first-line>" · <author> · <YYYY-MM-DD>
//
// The first-line message is truncated at 80 characters with an
// ellipsis when longer; the upstream message is already passed through
// devstatus.firstLine so embedded newlines do not appear. The author
// and date segments are independently elided when empty.
func renderCommit(cm parse.Commit) string {
	const maxMessageLen = 80

	var sb strings.Builder

	id := cm.ShortID
	if id == "" {
		id = cm.ID
	}
	if id == "" {
		id = "(commit)"
	}

	sb.WriteString("- ")
	if cm.URL != "" {
		sb.WriteString("[")
		sb.WriteString(id)
		sb.WriteString("](")
		sb.WriteString(cm.URL)
		sb.WriteString(")")
	} else {
		sb.WriteString(id)
	}
	if cm.Message != "" {
		msg := cm.Message
		if len(msg) > maxMessageLen {
			msg = msg[:maxMessageLen] + "…"
		}
		sb.WriteString(" — \"")
		sb.WriteString(msg)
		sb.WriteString("\"")
	}
	if cm.Author != "" {
		sb.WriteString(" · ")
		sb.WriteString(cm.Author)
	}
	if !cm.AuthoredAt.IsZero() {
		sb.WriteString(" · ")
		sb.WriteString(cm.AuthoredAt.UTC().Format("2006-01-02"))
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderRepository formats a single [parse.Repository] as one Markdown
// bullet line ending with a newline.
//
// Format:
//
//   - [<name>](<url>)
//
// This is the simplest of the five subsections: the Jira UI surfaces
// repositories that reference an issue even when no PR/branch/commit
// carries the issue key, so the renderer's job is to make the link
// visible. When URL is empty the name is rendered as plain text.
func renderRepository(rp parse.Repository) string {
	var sb strings.Builder
	name := rp.Name
	if name == "" {
		name = rp.URL
	}
	sb.WriteString("- ")
	if rp.URL != "" {
		sb.WriteString("[")
		sb.WriteString(name)
		sb.WriteString("](")
		sb.WriteString(rp.URL)
		sb.WriteString(")")
	} else {
		sb.WriteString(name)
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderBuild formats a single [parse.Build] as one Markdown bullet
// line ending with a newline.
//
// Format:
//
//   - [<STATE>] [<name>](<url>) — <YYYY-MM-DD> [tests <P>/<T>]
//
// The [tests P/T] suffix is omitted entirely when TestsTotal is 0 so
// builds from CI integrations that do not publish a test summary still
// render cleanly. State defaults to "UNKNOWN" when empty so the bullet
// still leads with the documented [STATE] marker.
func renderBuild(bd parse.Build) string {
	var sb strings.Builder

	state := bd.State
	if state == "" {
		state = "UNKNOWN"
	}
	name := bd.Name
	if name == "" {
		name = bd.ID
	}
	if name == "" {
		name = bd.URL
	}

	sb.WriteString("- [")
	sb.WriteString(state)
	sb.WriteString("] ")
	if bd.URL != "" {
		sb.WriteString("[")
		sb.WriteString(name)
		sb.WriteString("](")
		sb.WriteString(bd.URL)
		sb.WriteString(")")
	} else {
		sb.WriteString(name)
	}
	if !bd.LastUpdated.IsZero() {
		sb.WriteString(" — ")
		sb.WriteString(bd.LastUpdated.UTC().Format("2006-01-02"))
	}
	if bd.TestsTotal > 0 {
		sb.WriteString(fmt.Sprintf(" [tests %d/%d]", bd.TestsPassed, bd.TestsTotal))
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderPullRequest formats a single [parse.PullRequest] as one
// Markdown bullet line ending with a newline.
//
// Format:
//
//   - [<STATUS>] [<title>](<url>) — `<repository>` · <author>
//
// The repository and author segments are elided when empty so issues
// from providers that omit them still render cleanly. When both URL
// and title are empty the entry is rendered as a plain "- <ID>" line
// so devstatus's defensive non-URL entries do not vanish.
func renderPullRequest(pr parse.PullRequest) string {
	var sb strings.Builder

	status := pr.Status
	if status == "" {
		status = "UNKNOWN"
	}

	title := pr.Title
	if title == "" {
		title = pr.ID
	}
	if title == "" {
		title = pr.URL
	}

	sb.WriteString("- [")
	sb.WriteString(status)
	sb.WriteString("] ")

	if pr.URL != "" {
		sb.WriteString("[")
		sb.WriteString(title)
		sb.WriteString("](")
		sb.WriteString(pr.URL)
		sb.WriteString(")")
	} else {
		sb.WriteString(title)
	}

	if pr.Repository != "" {
		sb.WriteString(" — `")
		sb.WriteString(pr.Repository)
		sb.WriteString("`")
	}
	if pr.Author != "" {
		sb.WriteString(" · ")
		sb.WriteString(pr.Author)
	}
	sb.WriteString("\n")
	return sb.String()
}

// Custom-field value-kind classification constants used by
// classifyCustomField and the Custom fields section renderer.
//
// These are unexported because they are an implementation detail of
// the renderer's switch statement; callers do not select rendering
// behaviour by kind directly.
const (
	kindNull             = "null"
	kindPrimitive        = "primitive"
	kindStructured       = "structured"
	kindInvalid          = "invalid"
	kindStringStructured = "string-structured"
)

// classifyCustomField inspects the raw JSON bytes of a custom field
// value and decides how the renderer should format it.
//
// Returned values:
//   - kind:     one of kindNull, kindPrimitive, kindStructured,
//     kindInvalid, or kindStringStructured.
//   - pretty:   for kindStructured, the indented JSON produced by
//     json.Indent with prefix "" and indent "  ". For
//     kindPrimitive, the trimmed raw bytes as a string
//     (so JSON-string values keep their outer quotes for
//     reader clarity). For kindNull, the literal "null".
//     For kindInvalid, the trimmed raw bytes as a string
//     (preserving the original content verbatim). For
//     kindStringStructured, the decoded contents of the
//     JSON string with the outer JSON-string quotes
//     stripped — the inner structured text is the only
//     legible representation.
//   - indented: true when the pretty form spans more than one line.
//     This is the renderer's signal to wrap pretty in a
//     fenced code block; single-line structured values
//     ("[]", "{}", "[1]") render inline instead.
//
// Two-pass classification:
//
//  1. Outer pass. The raw bytes are inspected as JSON: null/empty →
//     kindNull, invalid → kindInvalid, object/array → kindStructured,
//     primitive other than string → kindPrimitive.
//  2. JSON-string inner pass. When the raw bytes are a JSON string
//     value (e.g. `"{repository={count=1, ...}, json={...}}"`),
//     classifyJSONStringContents inspects the decoded string contents
//     recursively. Possible outcomes:
//     a. inner is itself valid JSON object/array → kindStructured,
//     pretty-printed inner JSON (Jira sometimes encodes structured
//     values as transport-level strings),
//     b. inner is valid JSON but a primitive (e.g. `"\"hello\""`)
//     → kindPrimitive, the outer JSON-quoted string preserved so
//     the reader sees the quotes,
//     c. inner is not valid JSON but looks structured (contains
//     `{`, `[`, `=`, or '\n' — the canonical case is Atlassian's
//     customfield_10000 "Dev Status summary" blob) →
//     kindStringStructured, inner content verbatim with the outer
//     JSON-string quotes stripped,
//     d. inner is not valid JSON and looks like a plain short
//     string → kindPrimitive, outer quotes preserved.
//
// Edge cases handled:
//   - len(raw) == 0 is treated as kindNull. Jira does not normally
//     emit zero-byte field values, but the parser delivers an
//     empty RawMessage in some defensive paths and the renderer
//     must not panic.
//   - The historical fallback path for Atlassian's customfield_10000
//     blob being delivered as raw {key=value} bytes (not a JSON
//     string) is preserved as kindInvalid. The new path covers the
//     observed case where the same blob arrives JSON-string-encoded.
func classifyCustomField(raw json.RawMessage) (kind string, pretty string, indented bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return kindNull, "null", false
	}

	if !json.Valid(trimmed) {
		return kindInvalid, string(trimmed), bytes.ContainsRune(trimmed, '\n')
	}

	// JSON-string second pass: if raw decodes to a Go string,
	// inspect the inner content. This catches Jira tenants that
	// deliver structured payloads (most notably the Dev Status
	// summary) JSON-string-encoded.
	var asString string
	if err := json.Unmarshal(trimmed, &asString); err == nil {
		return classifyJSONStringContents(asString)
	}

	// Not a JSON string. The first non-whitespace byte
	// determines the JSON-level kind without re-parsing.
	switch trimmed[0] {
	case '{', '[':
		var buf bytes.Buffer
		if err := json.Indent(&buf, trimmed, "", "  "); err != nil {
			// json.Valid said yes; json.Indent says no. Surface
			// the raw bytes rather than silently swapping in "null".
			return kindInvalid, string(trimmed), bytes.ContainsRune(trimmed, '\n')
		}
		out := buf.String()
		return kindStructured, out, strings.Contains(out, "\n")
	default:
		// Primitive: number, true, false. (JSON strings handled
		// above by the json.Unmarshal-to-string fast path.)
		return kindPrimitive, string(trimmed), false
	}
}

// classifyJSONStringContents inspects the decoded contents of a JSON
// string custom-field value. It is called only when the outer raw
// bytes were successfully unmarshaled to a Go string; the four
// possible outcomes are documented on classifyCustomField.
//
// The pretty value is always callable directly as the body of either
// an inline "- label: <pretty>" line (kindPrimitive) or a fenced
// block (kindStructured, kindStringStructured): the renderer does
// not need to know which sub-case fired.
func classifyJSONStringContents(inner string) (kind string, pretty string, indented bool) {
	innerBytes := []byte(inner)

	// Case (a) and (b): the inner content is itself valid JSON.
	if json.Valid(innerBytes) {
		trimmedInner := bytes.TrimSpace(innerBytes)
		if isJSONStructured(trimmedInner) {
			var buf bytes.Buffer
			if err := json.Indent(&buf, trimmedInner, "", "  "); err == nil {
				out := buf.String()
				return kindStructured, out, strings.Contains(out, "\n")
			}
		}
		// Inner is valid JSON but a primitive (e.g. a string of a
		// string: "\"hello\""). A string of a string is just a
		// string; preserve the outer JSON-string quotes so the
		// reader can see the underlying value was a string rather
		// than digits or a literal.
		return kindPrimitive, strconv.Quote(inner), false
	}

	// Case (c): inner is not valid JSON, but looks structured.
	// The canonical example is Atlassian's customfield_10000
	// "Dev Status summary" blob in `{key=value, json={...}}` mixed
	// notation. Render the inner content verbatim — stripping the
	// outer JSON-string quotes is the only legible representation.
	if looksStructured(inner) {
		return kindStringStructured, inner, strings.ContainsRune(inner, '\n')
	}

	// Case (d): inner is a plain short string with no structural
	// characters. Render the OUTER JSON-quoted form inline; the
	// quotes are visual noise here but cheap, and consistent with
	// the kindPrimitive contract for non-string-encoded primitive
	// strings ("hello" → `"hello"` on the rendered line).
	return kindPrimitive, strconv.Quote(inner), false
}

// prettifyAtlassianBlob renders Atlassian's mixed
// Map.toString()+JSON notation across multiple indented lines
// while preserving every character of the original content.
//
// The format combines:
//   - Java Map.toString() syntax for outer wrapping:
//     {key=value, key=value}
//     with `=` as the separator and unquoted keys.
//   - Embedded real JSON inside a literal `json=` marker:
//     json={"key": "value", ...}
//     with `:` as the separator and quoted keys.
//
// The walker tracks brace depth and inserts:
//   - a newline + two-space indent after every "{" or "["
//   - a newline + two-space dedent before every "}" or "]"
//   - a newline + indent after every ", " at the current depth
//
// When the literal "json=" appears at a value position, the
// following balanced-brace JSON range is delegated to json.Indent
// at the current indent level. This yields proper JSON pretty-
// printing for the inner payload while preserving the outer
// Java-notation wrapping.
//
// The walker is single-pass and allocates one bytes.Buffer. It does
// NOT validate the input as JSON; the inner json= payload is
// validated lazily by json.Indent and on validation failure the
// whole function returns ok=false so the caller can fall back to
// verbatim rendering.
//
// On unbalanced braces, unexpected EOF, or an embedded json= range
// that fails json.Indent, the function returns the input unchanged
// with ok=false. The caller MUST check ok and fall back to verbatim
// on false; partial output is never returned.
//
// The two-space indent unit matches the rest of the renderer
// (json.Indent calls elsewhere also use two spaces).
//
// The caller is assumed to have already stripped any outer JSON-
// string quotes from the input. classifyCustomField's
// kindStringStructured branch does this via json.Unmarshal into a
// Go string before reaching the renderer.
func prettifyAtlassianBlob(s string) (pretty string, ok bool) {
	const indentUnit = "  "

	// writeIndent writes indentUnit repeated `depth` times.
	var buf bytes.Buffer
	writeIndent := func(depth int) {
		for i := 0; i < depth; i++ {
			buf.WriteString(indentUnit)
		}
	}

	depth := 0
	i := 0
	n := len(s)

	for i < n {
		c := s[i]

		// Recognise the literal `json=` marker followed by a JSON
		// value (`{...}` or `[...]`). This is the Atlassian Dev
		// Status summary's escape hatch into real JSON; we delegate
		// the embedded range to json.Indent at the current depth so
		// the inner payload pretty-prints in line with the outer
		// Java-notation wrapping.
		if c == 'j' && i+5 <= n && s[i:i+5] == "json=" {
			jvStart := i + 5
			if jvStart < n && (s[jvStart] == '{' || s[jvStart] == '[') {
				// Scan forward to the balanced closing brace,
				// respecting JSON string literals.
				end, sok := findJSONValueEnd(s, jvStart)
				if !sok {
					return s, false
				}
				rawJSON := s[jvStart:end]

				// Compose the current indent prefix for json.Indent.
				// Every continuation line of the pretty JSON is
				// prepended with this prefix; the first line is
				// written immediately after `json=` and inherits no
				// prefix (json.Indent does not prefix the first
				// line).
				var prefix strings.Builder
				for k := 0; k < depth; k++ {
					prefix.WriteString(indentUnit)
				}

				var indented bytes.Buffer
				if err := json.Indent(&indented, []byte(rawJSON), prefix.String(), indentUnit); err != nil {
					return s, false
				}

				buf.WriteString("json=")
				buf.Write(indented.Bytes())
				i = end
				continue
			}
		}

		switch c {
		case '{', '[':
			// Empty pair (`{}` or `[]`) collapses to a single token
			// so trivial containers do not balloon into three lines.
			// Peek the very next byte — Atlassian's notation never
			// inserts whitespace inside an empty container, so the
			// peek is exact, not a tolerance.
			closer := byte('}')
			if c == '[' {
				closer = ']'
			}
			if i+1 < n && s[i+1] == closer {
				buf.WriteByte(c)
				buf.WriteByte(closer)
				i += 2
				continue
			}
			buf.WriteByte(c)
			depth++
			buf.WriteByte('\n')
			writeIndent(depth)
			i++
		case '}', ']':
			depth--
			if depth < 0 {
				return s, false
			}
			buf.WriteByte('\n')
			writeIndent(depth)
			buf.WriteByte(c)
			i++
		case ',':
			// Only act on ", " (comma followed by a space) at the
			// current depth — that is the Java-notation key/value
			// separator. A bare comma inside an embedded json=
			// payload is consumed by json.Indent above; we will
			// never see one here in normal input.
			if i+1 < n && s[i+1] == ' ' {
				buf.WriteByte(',')
				buf.WriteByte('\n')
				writeIndent(depth)
				i += 2
				continue
			}
			buf.WriteByte(c)
			i++
		default:
			buf.WriteByte(c)
			i++
		}
	}

	if depth != 0 {
		return s, false
	}
	return buf.String(), true
}

// findJSONValueEnd scans forward from start (which must point at a
// '{' or '[' byte) and returns the exclusive end index of the
// balanced JSON value. It tracks JSON string literals so that braces
// inside `"..."` are not counted.
//
// Returns ok=false on unbalanced input or unexpected EOF. The walker
// uses this to identify the byte range to hand off to json.Indent.
func findJSONValueEnd(s string, start int) (end int, ok bool) {
	n := len(s)
	if start >= n {
		return 0, false
	}
	first := s[start]
	if first != '{' && first != '[' {
		return 0, false
	}

	depth := 0
	inString := false
	escape := false

	for i := start; i < n; i++ {
		c := s[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

// looksStructured reports whether a JSON-string value's decoded
// contents are structured enough to warrant rendering in a fenced
// block, even though the contents themselves are not valid JSON.
//
// Heuristic: a string is "structured" if it contains at least one
// of '{', '[', '=', or '\n'. This catches the three observed cases:
//
//   - Atlassian's Dev Status summary
//     ({repository={count=1, ...}, json={...}}) — multiple `{` and
//     `=`,
//   - JSON-stringified-twice payloads that decode to JSON-like text
//     but did not survive the double-encoding — `{`/`[`,
//   - multi-line audit-log payloads stuffed into a single field —
//     '\n'.
//
// Single-line short strings without any of these characters render
// inline as primitives; wrapping them in a fenced block would be
// visual noise.
func looksStructured(s string) bool {
	return strings.ContainsAny(s, "{[=\n")
}

// isJSONStructured reports whether the given (already-validated)
// JSON bytes represent a JSON object or array — i.e. begin with
// '{' or '['. Used by the classifier to decide whether to invoke
// json.Indent at all.
func isJSONStructured(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	return b[0] == '{' || b[0] == '['
}

// labelFor returns the Markdown label for a custom field id, using
// the human-readable name from the names map when available and
// falling back to the raw id in backticks otherwise.
//
// The bold-vs-backtick distinction is intentional: a known label is
// a real word ("**Sprint**", "**Rank**") and renders cleanly as
// strong text; an unknown id is a code-like token
// ("`customfield_10115`") and renders cleanly as inline code.
// Mixing the two in one rendered section is fine — it visually
// flags which fields the Jira tenant has bothered to name.
func labelFor(id string, names map[string]string) string {
	if name, ok := names[id]; ok && name != "" {
		return "**" + name + "**"
	}
	return "`" + id + "`"
}

// indentByTwo prefixes every line of s with two spaces. It is used
// to nest fenced code blocks under Markdown list bullets so the
// fence (` ``` `) and its content sit at the bullet's content
// column. Without this prefixing, common Markdown renderers
// (GitHub, GitLab, Obsidian) treat the fence as terminating the
// surrounding list, which collapses the layout.
func indentByTwo(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "  " + line
	}
	return strings.Join(lines, "\n")
}
