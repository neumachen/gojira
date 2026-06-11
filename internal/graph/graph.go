// Package graph is a pure in-memory model of the Jira issue graph
// discovered during a gojira crawl. It exposes a [Collector] that
// accumulates nodes and edges from parsed Jira issues plus their
// extracted outbound references, and two renderers — [RenderJSON]
// (machine-readable) and [RenderD2] (the D2 diagram language source).
//
// The package is deliberately pure: no I/O, no network, no project-
// internal imports beyond the parse/extract/classify data carriers.
// Callers (internal/crawl) are responsible for persisting the
// rendered output. Both renderers produce deterministic, sorted
// output so they are safe to use as golden-test fixtures.
package graph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
)

// NodeKind labels the kind of a graph node.
type NodeKind string

const (
	// NodeIssue is a Jira issue node, identified by its UPPER issue key.
	NodeIssue NodeKind = "issue"

	// NodeGitHubPR is a GitHub pull request node, identified by
	// "owner/repo#N" when the URL is parseable, or by its raw URL otherwise.
	NodeGitHubPR NodeKind = "github_pr"

	// NodeExternal is any other URL reference, identified by the URL itself.
	NodeExternal NodeKind = "external"
)

// EdgeKind labels the kind of a graph edge.
type EdgeKind string

const (
	// EdgeParent connects an issue to its parent issue.
	EdgeParent EdgeKind = "parent"
	// EdgeSubtask connects an issue to one of its subtasks.
	EdgeSubtask EdgeKind = "subtask"
	// EdgeChild connects an issue to a hierarchy child discovered via JQL.
	EdgeChild EdgeKind = "child"
	// EdgeLink connects two issues via a structured Jira issue link.
	EdgeLink EdgeKind = "link"
	// EdgeRemote connects an issue to one of its remote (external) links.
	EdgeRemote EdgeKind = "remote"
	// EdgeDescription connects an issue to a reference found in its description body.
	EdgeDescription EdgeKind = "description"
	// EdgePullRequest connects an issue to a GitHub pull request node.
	EdgePullRequest EdgeKind = "pull_request"
	// EdgeExternal connects an issue to an external (non-Jira / non-PR) URL.
	EdgeExternal EdgeKind = "external"
)

// Node is a single graph node.
type Node struct {
	// ID is the canonical, unique identifier. Issue: UPPER issue key
	// ("PROJ-1"). GitHub PR: "owner/repo#N" when parseable, else the URL.
	// External: the URL itself.
	ID   string   `json:"id"`
	Kind NodeKind `json:"kind"`
	// Label is the human-readable label used in the D2 diagram.
	Label string `json:"label"`
	// Issue-only optional metadata; omitted when zero.
	Status   string `json:"status,omitempty"`
	Type     string `json:"type,omitempty"`
	Assignee string `json:"assignee,omitempty"`
	URL      string `json:"url,omitempty"`
	// Fetched is true if this issue was actually fetched/rendered in
	// the crawl; false for nodes that exist only as references from
	// other issues. PR/external nodes are always false.
	Fetched bool `json:"fetched"`
}

// Edge is a directed relationship between two nodes.
type Edge struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Kind  EdgeKind `json:"kind"`
	Label string   `json:"label,omitempty"`
}

// Model is the assembled, deterministic result of a [Collector] run.
// Nodes are sorted by ID; Edges are sorted by (From, To, Kind, Label).
type Model struct {
	Nodes []Node
	Edges []Edge
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

// Collector accumulates nodes and edges as issues are processed.
// Methods are NOT safe for concurrent use; callers must serialize
// access (the crawler invokes the collector while holding c.mu).
type Collector struct {
	nodes map[string]Node
	// edgeSet is keyed by (From, To, Kind) so the same structured
	// edge cannot be added twice and so a structured edge (parent /
	// subtask / child / link / remote / pull_request / external)
	// can suppress a description-source edge between the same pair.
	edgeSet map[edgeKey]Edge
}

// edgeKey is the (From, To, Kind) tuple used for edge dedup.
type edgeKey struct {
	from string
	to   string
	kind EdgeKind
}

// NewCollector returns an empty Collector ready to accumulate.
func NewCollector() *Collector {
	return &Collector{
		nodes:   map[string]Node{},
		edgeSet: map[edgeKey]Edge{},
	}
}

// Add records issue and all relationships derived from issue + refs.
// Calling Add for an issue.Key implies that issue was fetched
// (sets Fetched=true and populates the issue-node metadata).
func (c *Collector) Add(issue parse.Issue, refs []extract.Reference) {
	key := strings.ToUpper(strings.TrimSpace(issue.Key))
	if key == "" {
		return
	}

	// (1) The fetched issue node — populate / overwrite metadata.
	n := Node{
		ID:       key,
		Kind:     NodeIssue,
		Label:    issueLabel(key, issue.Summary),
		Status:   issue.Status,
		Type:     issue.IssueType,
		Assignee: issue.Assignee,
		URL:      issue.SourceURL,
		Fetched:  true,
	}
	c.upsertNode(n)

	// (2) Structured relationships first — they take precedence over
	// description-source edges between the same pair.

	if issue.Parent != nil && issue.Parent.Key != "" {
		pkey := strings.ToUpper(strings.TrimSpace(issue.Parent.Key))
		c.upsertNode(referencedIssue(pkey, issue.Parent.Summary))
		c.addEdge(key, pkey, EdgeParent, "")
	}

	for _, st := range issue.Subtasks {
		if st.Key == "" {
			continue
		}
		skey := strings.ToUpper(strings.TrimSpace(st.Key))
		c.upsertNode(referencedIssue(skey, st.Summary))
		c.addEdge(key, skey, EdgeSubtask, "")
	}

	for _, ck := range issue.Children {
		if ck == "" {
			continue
		}
		cKey := strings.ToUpper(strings.TrimSpace(ck))
		c.upsertNode(referencedIssue(cKey, ""))
		c.addEdge(key, cKey, EdgeChild, "")
	}

	for _, il := range issue.IssueLinks {
		if il.Key == "" {
			continue
		}
		lkey := strings.ToUpper(strings.TrimSpace(il.Key))
		c.upsertNode(referencedIssue(lkey, il.Summary))
		label := strings.TrimSpace(strings.ToLower(il.Type))
		// Direction qualifier is informative; keep the bare type to
		// keep labels short ("blocks", "relates"). Callers wanting
		// the direction can re-derive from the edge direction itself.
		c.addEdge(key, lkey, EdgeLink, label)
	}

	for _, rl := range issue.RemoteLinks {
		if rl.URL == "" {
			continue
		}
		c.upsertNode(externalNode(rl.URL, rl.Title))
		c.addEdge(key, rl.URL, EdgeRemote, rl.Title)
	}

	for _, pr := range issue.DevStatus.PullRequests {
		if pr.URL == "" {
			continue
		}
		id, label := prID(pr.URL, "", "", 0)
		c.upsertNode(Node{
			ID: id, Kind: NodeGitHubPR, Label: label, URL: pr.URL,
		})
		c.addEdge(key, id, EdgePullRequest, "")
	}

	// (3) Refs — dedup against structured edges by (From,To,Kind).
	for _, ref := range refs {
		switch ref.Kind {
		case classify.KindJiraKey, classify.KindJiraURL:
			if ref.IssueKey == "" {
				continue
			}
			rkey := strings.ToUpper(strings.TrimSpace(ref.IssueKey))
			// Suppress a description-source duplicate if a
			// structured edge already connects the same pair.
			if c.anyEdgeBetween(key, rkey) {
				continue
			}
			c.upsertNode(referencedIssue(rkey, ref.Text))
			c.addEdge(key, rkey, EdgeDescription, "")
		case classify.KindGitHubPR:
			id, label := prID(ref.URL,
				ref.ClassifyResult.Owner,
				ref.ClassifyResult.Repo,
				ref.ClassifyResult.PRNumber)
			c.upsertNode(Node{
				ID: id, Kind: NodeGitHubPR, Label: label, URL: ref.URL,
			})
			c.addEdge(key, id, EdgePullRequest, "")
		case classify.KindExternal:
			if ref.URL == "" {
				continue
			}
			c.upsertNode(externalNode(ref.URL, ref.Text))
			c.addEdge(key, ref.URL, EdgeExternal, ref.Text)
		}
	}
}

// MarkFetched records that an issue was actually fetched. It creates
// a placeholder issue node if none exists yet, or flips an existing
// referenced placeholder to Fetched=true.
func (c *Collector) MarkFetched(key string) {
	k := strings.ToUpper(strings.TrimSpace(key))
	if k == "" {
		return
	}
	n, ok := c.nodes[k]
	if !ok {
		n = referencedIssue(k, "")
	}
	n.Fetched = true
	c.nodes[k] = n
}

// Model returns the assembled, deterministic Model. It is safe to
// call multiple times; each call produces an independently-sorted
// snapshot.
func (c *Collector) Model() Model {
	nodes := make([]Node, 0, len(c.nodes))
	for _, n := range c.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].ID < nodes[j].ID
	})

	edges := make([]Edge, 0, len(c.edgeSet))
	for _, e := range c.edgeSet {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Label < b.Label
	})

	return Model{Nodes: nodes, Edges: edges}
}

// upsertNode inserts a node or merges into an existing one. The
// merge rule preserves the most specific metadata: a Fetched=true
// node always wins; otherwise non-empty fields from the new node
// overwrite empty fields on the stored node.
func (c *Collector) upsertNode(n Node) {
	if n.ID == "" {
		return
	}
	cur, ok := c.nodes[n.ID]
	if !ok {
		c.nodes[n.ID] = n
		return
	}
	// Preserve the existing Kind if it was already issue/pr/external.
	// (We never demote a fetched issue back to a placeholder.)
	merged := cur
	if !merged.Fetched && n.Fetched {
		merged.Fetched = true
	}
	if merged.Kind == "" {
		merged.Kind = n.Kind
	}
	merged.Label = preferNonEmpty(n.Label, merged.Label)
	merged.Status = preferNonEmpty(n.Status, merged.Status)
	merged.Type = preferNonEmpty(n.Type, merged.Type)
	merged.Assignee = preferNonEmpty(n.Assignee, merged.Assignee)
	merged.URL = preferNonEmpty(n.URL, merged.URL)
	c.nodes[n.ID] = merged
}

// addEdge inserts an edge keyed by (From,To,Kind). If an edge with the
// same key already exists, the existing edge (and its label) is
// preserved — first writer wins for label.
func (c *Collector) addEdge(from, to string, k EdgeKind, label string) {
	if from == "" || to == "" {
		return
	}
	key := edgeKey{from: from, to: to, kind: k}
	if _, ok := c.edgeSet[key]; ok {
		return
	}
	c.edgeSet[key] = Edge{From: from, To: to, Kind: k, Label: label}
}

// anyEdgeBetween reports whether any structured (i.e. non-description)
// edge already connects from→to.
func (c *Collector) anyEdgeBetween(from, to string) bool {
	for _, k := range []EdgeKind{
		EdgeParent, EdgeSubtask, EdgeChild,
		EdgeLink, EdgeRemote, EdgePullRequest, EdgeExternal,
	} {
		if _, ok := c.edgeSet[edgeKey{from: from, to: to, kind: k}]; ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Renderers
// ---------------------------------------------------------------------------

// jsonEnvelope is the wire shape RenderJSON marshals. Keeping the
// envelope explicit makes the schema version observable.
type jsonEnvelope struct {
	Version int    `json:"version"`
	Nodes   []Node `json:"nodes"`
	Edges   []Edge `json:"edges"`
}

// RenderJSON returns the indented JSON representation of m. The output
// is deterministic because Model() pre-sorts.
func RenderJSON(m Model) ([]byte, error) {
	env := jsonEnvelope{Version: 1, Nodes: m.Nodes, Edges: m.Edges}
	if env.Nodes == nil {
		env.Nodes = []Node{}
	}
	if env.Edges == nil {
		env.Edges = []Edge{}
	}
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, errext.Errorf("graph: marshal json: %w", err)
	}
	return out, nil
}

// RenderD2 returns valid D2 source describing m. The output:
//   - starts with a comment header documenting how to render the file,
//   - declares one D2 shape per node with a quoted key + quoted label,
//   - declares one D2 connection per edge in deterministic order.
func RenderD2(m Model) (string, error) {
	var b strings.Builder
	b.WriteString("# gojira issue graph (D2 source) — render with: d2 graph.d2 graph.svg\n")
	b.WriteString("\n")

	// Nodes.
	for _, n := range m.Nodes {
		b.WriteString(d2Quote(n.ID))
		b.WriteString(": {\n")
		b.WriteString("  label: ")
		b.WriteString(d2Quote(n.Label))
		b.WriteString("\n")
		switch n.Kind {
		case NodeGitHubPR:
			b.WriteString("  shape: hexagon\n")
		case NodeExternal:
			b.WriteString("  shape: page\n")
		}
		if n.Kind == NodeIssue && !n.Fetched {
			b.WriteString("  style.stroke-dash: 3\n")
		}
		b.WriteString("}\n")
	}

	if len(m.Nodes) > 0 && len(m.Edges) > 0 {
		b.WriteString("\n")
	}

	// Edges.
	for _, e := range m.Edges {
		b.WriteString(d2Quote(e.From))
		b.WriteString(" -> ")
		b.WriteString(d2Quote(e.To))
		if e.Label != "" {
			b.WriteString(": ")
			b.WriteString(d2Quote(e.Label))
		}
		b.WriteString("\n")
	}

	return b.String(), nil
}

// d2Quote returns a D2-quoted string. Backslashes and double-quotes are
// escaped so the resulting token is always a valid quoted D2 identifier
// regardless of the content (special chars #, /, spaces, : are
// fine inside a quoted key).
func d2Quote(s string) string {
	if s == "" {
		return `""`
	}
	// Replace \ first so we don't double-escape later replacements.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func preferNonEmpty(new, old string) string {
	if new != "" {
		return new
	}
	return old
}

// referencedIssue returns a placeholder issue node (Fetched=false)
// suitable for upsertNode. Summary is used to seed Label even though
// the issue itself has not been Add'd yet.
func referencedIssue(key, summary string) Node {
	return Node{
		ID:    key,
		Kind:  NodeIssue,
		Label: issueLabel(key, summary),
	}
}

// externalNode returns an external-URL node. title is used as the
// label fallback; the URL itself is used when title is empty.
func externalNode(url, title string) Node {
	label := title
	if label == "" {
		label = url
	}
	return Node{
		ID:    url,
		Kind:  NodeExternal,
		Label: label,
		URL:   url,
	}
}

// issueLabel returns "KEY: summary" or just KEY when summary is empty.
// The summary is truncated at the first newline and to at most 80
// characters so the D2 diagram stays legible.
func issueLabel(key, summary string) string {
	s := strings.TrimSpace(summary)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:77] + "…"
	}
	if s == "" {
		return key
	}
	return key + ": " + s
}

// ghPullRegexp matches the canonical GitHub PR URL form, ignoring
// optional trailing path segments, query, and fragment.
var ghPullRegexp = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// prID returns the canonical PR node id and its label. It prefers
// the (owner, repo, n) triple from a pre-computed [classify.Result]
// when available, falling back to parsing the URL itself, and
// finally to the raw URL when neither yields a structured form.
func prID(url, owner, repo string, n int) (string, string) {
	if owner != "" && repo != "" && n > 0 {
		id := fmt.Sprintf("%s/%s#%d", owner, repo, n)
		return id, id
	}
	if m := ghPullRegexp.FindStringSubmatch(url); m != nil {
		nn, err := strconv.Atoi(m[3])
		if err == nil && nn > 0 {
			id := fmt.Sprintf("%s/%s#%d", m[1], m[2], nn)
			return id, id
		}
	}
	if url != "" {
		return url, url
	}
	return "", ""
}
