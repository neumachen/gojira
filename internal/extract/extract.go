// Package extract discovers all outbound references from a parsed Jira issue.
//
// It is a pure package: no network calls, no filesystem access. It composes
// three leaf packages — internal/parse (typed Issue), internal/adf (ADF link
// traversal), and classify (URL/key classification) — and produces a flat
// slice of Reference values that the crawl orchestrator can act on.
//
// # Allowed imports
//
// Only the Go standard library, internal/parse, internal/adf, and classify.
// This package must never import internal/client, internal/output,
// internal/events, or internal/render.
package extract

import (
	"github.com/neumachen/errext"
	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/internal/adf"
	"github.com/neumachen/gojira/internal/parse"
)

// Source identifies where in a Jira issue a Reference was discovered.
// Future sources (comments, development metadata) can be added without
// breaking existing callers because Source is an opaque int.
type Source int

const (
	// SourceDescription means the reference was found inside the ADF
	// description field of the issue (via adf.ExtractLinks).
	SourceDescription Source = iota

	// SourceRelationship means the reference came from a structured
	// relationship field: the parent issue, a subtask, or an issue link.
	SourceRelationship

	// SourceRemoteLink means the reference came from Jira's remote-links
	// list (the /remotelink endpoint or the embedded remotelinks field).
	SourceRemoteLink
)

// String returns a human-readable label for the Source, useful for debugging.
func (s Source) String() string {
	switch s {
	case SourceDescription:
		return "Description"
	case SourceRelationship:
		return "Relationship"
	case SourceRemoteLink:
		return "RemoteLink"
	default:
		return "Unknown"
	}
}

// Reference is a single outbound reference discovered in a Jira issue.
//
// # GitHub PR access
//
// When Kind is classify.KindGitHubPR, the full classify.Result is embedded in
// ClassifyResult, giving downstream consumers direct access to Owner, Repo,
// and PRNumber without re-classifying the URL. For all other Kinds the same
// field carries the classification result (IssueKey for KindJiraURL, URL for
// KindExternal, etc.).
//
// # Relation field
//
// Relation is non-empty only for SourceRelationship references that originate
// from an IssueLink. Its value is "<Direction> <Type>", for example
// "outward Blocks" or "inward Relates". For parent and subtask references,
// Relation is empty because the relationship type is implicit in the structure.
// For SourceDescription and SourceRemoteLink references, Relation is always
// empty.
type Reference struct {
	// Kind is the classification of this reference.
	Kind classify.Kind

	// IssueKey is the Jira issue key (e.g. "EXAMPLE-1").
	// Populated when Kind is KindJiraKey or KindJiraURL.
	IssueKey string

	// URL is the raw URL string.
	// Populated when Kind is KindJiraURL, KindGitHubPR, or KindExternal.
	URL string

	// Text is the visible label for this reference, when available.
	// For ADF description links: the link mark's text node content.
	// For parent/subtask/issuelink: the linked issue's Summary.
	// For remote links: the remote link's Title field.
	Text string

	// Source identifies which part of the issue this reference came from.
	Source Source

	// Relation carries the link direction and type for IssueLink references,
	// e.g. "outward Blocks". Empty for all other sources.
	Relation string

	// ClassifyResult is the full result from classify.Classify. It is always
	// populated. Downstream consumers can use it to access Owner, Repo, and
	// PRNumber for KindGitHubPR references without re-classifying the URL.
	ClassifyResult classify.Result
}

// Extract discovers all outbound references from issue and returns them as a
// flat slice. jiraSite is the Jira Cloud base URL (e.g.
// "https://example.atlassian.net") passed through to adf.ExtractLinks and
// classify.Classify for URL classification.
//
// # Return order (stable contract)
//
// References are returned in the following order:
//  1. Description links — in document order as returned by adf.ExtractLinks.
//  2. Parent — if the issue has a parent, exactly one reference.
//  3. Subtasks — in the order they appear in issue.Subtasks.
//  4. Issue links — in the order they appear in issue.IssueLinks.
//  5. Remote links — in the order they appear in issue.RemoteLinks.
//
// This ordering is deterministic given the same input and is documented so
// that callers (e.g. crawl, render) can rely on it.
//
// # Deduplication
//
// Extract does NOT deduplicate references. The same URL or issue key may
// appear more than once if it is referenced from multiple sources. The crawl
// orchestrator owns deduplication because it needs the full reference graph.
//
// # Malformed ADF description
//
// If adf.ExtractLinks returns an error (e.g. the description is not valid ADF
// JSON), Extract returns that error immediately and emits no references. The
// caller should treat this as a non-fatal issue-level error and decide whether
// to skip the issue or surface it as a warning.
//
// # Empty result
//
// When an issue has no references of any kind, Extract returns a non-nil empty
// slice ([]Reference{}) and a nil error.
func Extract(issue parse.Issue, jiraSite string) ([]Reference, error) {
	refs := make([]Reference, 0)

	// 1. ADF description links.
	adfLinks, err := adf.ExtractLinks(issue.Description, jiraSite)
	if err != nil {
		return nil, errext.Errorf("extract: ADF description for %s: %w", issue.Key, err)
	}
	for _, l := range adfLinks {
		refs = append(refs, Reference{
			Kind:           l.Classification.Kind,
			IssueKey:       l.Classification.IssueKey,
			URL:            l.Classification.URL,
			Text:           l.Text,
			Source:         SourceDescription,
			ClassifyResult: l.Classification,
		})
	}

	// 2. Parent.
	if issue.Parent != nil {
		refs = append(refs, Reference{
			Kind:     classify.KindJiraKey,
			IssueKey: issue.Parent.Key,
			Text:     issue.Parent.Summary,
			Source:   SourceRelationship,
			ClassifyResult: classify.Result{
				Kind:     classify.KindJiraKey,
				IssueKey: issue.Parent.Key,
			},
		})
	}

	// 3. Subtasks.
	for _, st := range issue.Subtasks {
		refs = append(refs, Reference{
			Kind:     classify.KindJiraKey,
			IssueKey: st.Key,
			Text:     st.Summary,
			Source:   SourceRelationship,
			ClassifyResult: classify.Result{
				Kind:     classify.KindJiraKey,
				IssueKey: st.Key,
			},
		})
	}

	// 4. Issue links.
	for _, il := range issue.IssueLinks {
		refs = append(refs, Reference{
			Kind:     classify.KindJiraKey,
			IssueKey: il.Key,
			Text:     il.Summary,
			Source:   SourceRelationship,
			Relation: il.Direction + " " + il.Type,
			ClassifyResult: classify.Result{
				Kind:     classify.KindJiraKey,
				IssueKey: il.Key,
			},
		})
	}

	// 5. Remote links.
	for _, rl := range issue.RemoteLinks {
		cr := classify.Classify(rl.URL, jiraSite)
		refs = append(refs, Reference{
			Kind:           cr.Kind,
			IssueKey:       cr.IssueKey,
			URL:            cr.URL,
			Text:           rl.Title,
			Source:         SourceRemoteLink,
			ClassifyResult: cr,
		})
	}

	return refs, nil
}
