package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/internal/extract"
	"github.com/neumachen/gojira/internal/graph"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/neumachen/gojira/pkg/classify"
	"github.com/neumachen/gojira/pkg/client"
)

// bridgeBackend implements [mcpBackend] in "bridge" mode by forwarding
// every call to a remote `gojira serve` gRPC server. It owns the
// generated [gojirav1.GojiraClient]; the caller owns the underlying
// *grpc.ClientConn lifecycle via the returned closer.
type bridgeBackend struct {
	client gojirav1.GojiraClient
}

// NewBridgeBackend dials the supplied gRPC address using plaintext
// credentials (the bridge target is local loopback by design; auth
// and TLS are explicitly out of scope per Phase B's PRD). It returns
// the constructed backend, a closer that drops the underlying
// connection (deferrable by the caller), and a dial error.
//
// NewBridgeBackend does NOT block waiting for the server to be
// reachable — grpc.NewClient is non-blocking; per-RPC errors surface
// the unreachable case at first use, matching the "lazy" client
// behavior used by cmd/gojira-client/main.go.
func NewBridgeBackend(addr string) (*bridgeBackend, func() error, error) {
	if addr == "" {
		return nil, nil, errors.New("bridge backend: server address is required")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("bridge backend: dial %s: %w", addr, err)
	}
	return newBridgeBackendFromConn(conn), conn.Close, nil
}

// newBridgeBackendFromConn is the test seam used by bridge_backend_test.go
// to inject a bufconn-backed *grpc.ClientConn. Production code uses
// NewBridgeBackend (which calls this helper after a successful dial).
func newBridgeBackendFromConn(cc *grpc.ClientConn) *bridgeBackend {
	return &bridgeBackend{client: gojirav1.NewGojiraClient(cc)}
}

// ---------------------------------------------------------------------------
// Read paths
// ---------------------------------------------------------------------------

func (b *bridgeBackend) Classify(ctx context.Context, input, jiraSite string) (classify.Result, error) {
	resp, err := b.client.Classify(ctx, &gojirav1.ClassifyRequest{
		Input:    input,
		JiraSite: jiraSite,
	})
	if err != nil {
		return classify.Result{}, err
	}
	kind, _ := classifyKindFromString(resp.GetKind())
	return classify.Result{
		Kind:     kind,
		IssueKey: resp.GetIssueKey(),
		Owner:    resp.GetOwner(),
		Repo:     resp.GetRepo(),
		PRNumber: int(resp.GetPrNumber()),
		URL:      resp.GetUrl(),
	}, nil
}

// GetIssue forwards to the gRPC GetIssue RPC and converts the proto
// Issue back into the parse.Issue / extract.Reference shape the
// facade backend returns. The bridge cannot reconstruct every field
// the facade exposes (custom fields, ADF body, etc.) — only the
// fields the proto carries — so this is a best-effort projection
// suitable for the MCP get_issue tool's JSON output.
func (b *bridgeBackend) GetIssue(ctx context.Context, key string) (parse.Issue, []extract.Reference, error) {
	resp, err := b.client.GetIssue(ctx, &gojirav1.GetIssueRequest{Key: key})
	if err != nil {
		return parse.Issue{}, nil, err
	}
	pi := resp.GetIssue()
	if pi == nil {
		return parse.Issue{}, nil, errors.New("bridge backend: GetIssue returned no issue")
	}
	out := parse.Issue{
		Key:       pi.GetKey(),
		NumericID: pi.GetNumericId(),
		Summary:   pi.GetSummary(),
		Status:    pi.GetStatus(),
		IssueType: pi.GetIssueType(),
		Assignee:  pi.GetAssignee(),
		Reporter:  pi.GetReporter(),
		SourceURL: pi.GetSourceUrl(),
		Children:  append([]string(nil), pi.GetChildren()...),
	}
	if t := pi.GetCreated(); t != nil {
		out.Created = t.AsTime()
	}
	if t := pi.GetUpdated(); t != nil {
		out.Updated = t.AsTime()
	}
	// References: shallow projection — preserve Kind + IssueKey/URL
	// so the MCP tool's JSON output carries the same "outbound graph"
	// the self-mode handler shows.
	refs := make([]extract.Reference, 0, len(pi.GetReferences()))
	for _, r := range pi.GetReferences() {
		kind, _ := classifyKindFromString(r.GetKind())
		refs = append(refs, extract.Reference{
			Kind:     kind,
			IssueKey: r.GetIssueKey(),
			URL:      r.GetUrl(),
			Text:     r.GetTitle(),
			Relation: r.GetRelation(),
		})
	}
	return out, refs, nil
}

// Crawl opens the streaming Crawl RPC, drains it, and translates
// KIND_ISSUE_FETCHED events into [ProgressFn] callbacks while
// capturing the terminal KIND_CRAWL_SUMMARY event into the returned
// [gojira.Summary]. A non-EOF stream error aborts; an EOF without a
// summary event yields a zero-valued Summary and no error (the
// crawl orchestrator emits the summary unless it crashed).
func (b *bridgeBackend) Crawl(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, error) {
	if progress == nil {
		progress = noopProgress
	}
	stream, err := b.client.Crawl(ctx, &gojirav1.CrawlRequest{StartKeys: startKeys})
	if err != nil {
		return gojira.Summary{}, err
	}
	var done int
	var summary gojira.Summary
	for {
		evt, err := stream.Recv()
		if err == io.EOF {
			return summary, nil
		}
		if err != nil {
			return summary, err
		}
		switch evt.GetKind() {
		case gojirav1.CrawlEvent_KIND_ISSUE_FETCHED:
			done++
			progress(done, done, fmt.Sprintf("fetched %s", evt.GetIssueKey()))
		case gojirav1.CrawlEvent_KIND_CRAWL_SUMMARY:
			if s := evt.GetSummary(); s != nil {
				summary = gojira.Summary{
					Fetched:     int(s.GetFetched()),
					Skipped:     int(s.GetSkipped()),
					Stubbed:     int(s.GetStubbed()),
					Failed:      int(s.GetFailed()),
					CapLimited:  int(s.GetCapLimited()),
					PRsFound:    int(s.GetPrsFound()),
					FetchedKeys: append([]string(nil), s.GetFetchedKeys()...),
					StubbedKeys: append([]string(nil), s.GetStubbedKeys()...),
					Duration:    time.Duration(s.GetDurationMs()) * time.Millisecond,
				}
				if fk := s.GetFailedKeys(); len(fk) > 0 {
					summary.FailedKeys = make(map[string]string, len(fk))
					for k, v := range fk {
						summary.FailedKeys[k] = v
					}
				}
			}
		}
	}
}

// GetGraph forwards to the unary GetGraph RPC and maps the proto
// response into a [gojira.GraphModel]. Progress is not driven by
// this path (the unary RPC is the wrong place to surface streamed
// progress); the supplied progress callback is invoked exactly once
// with the terminal node count when the call returns.
func (b *bridgeBackend) GetGraph(ctx context.Context, startKeys []string, progress ProgressFn) (gojira.Summary, gojira.GraphModel, error) {
	if progress == nil {
		progress = noopProgress
	}
	resp, err := b.client.GetGraph(ctx, &gojirav1.GetGraphRequest{StartKeys: startKeys})
	if err != nil {
		return gojira.Summary{}, gojira.GraphModel{}, err
	}
	model := gojira.GraphModel{
		Nodes: make([]gojira.GraphNode, 0, len(resp.GetNodes())),
		Edges: make([]gojira.GraphEdge, 0, len(resp.GetEdges())),
	}
	for _, n := range resp.GetNodes() {
		model.Nodes = append(model.Nodes, gojira.GraphNode{
			ID:       n.GetId(),
			Kind:     graph.NodeKind(n.GetKind()),
			Label:    n.GetLabel(),
			Status:   n.GetStatus(),
			Type:     n.GetType(),
			Assignee: n.GetAssignee(),
			URL:      n.GetUrl(),
			Fetched:  n.GetFetched(),
		})
	}
	for _, e := range resp.GetEdges() {
		model.Edges = append(model.Edges, gojira.GraphEdge{
			From:  e.GetFrom(),
			To:    e.GetTo(),
			Kind:  graph.EdgeKind(e.GetKind()),
			Label: e.GetLabel(),
		})
	}
	progress(len(model.Nodes), len(model.Nodes), "graph ready")
	return gojira.Summary{}, model, nil
}

// ---------------------------------------------------------------------------
// Write paths
// ---------------------------------------------------------------------------

func (b *bridgeBackend) CreateIssue(ctx context.Context, project, issueType string, fields CreateIssueFields) (client.CreatedIssue, error) {
	rawFields, err := rawFieldsToProto(fields.RawFields)
	if err != nil {
		return client.CreatedIssue{}, err
	}
	resp, err := b.client.CreateIssue(ctx, &gojirav1.CreateIssueRequest{
		Project:     project,
		IssueType:   issueType,
		Summary:     fields.Summary,
		Description: fields.Description,
		Labels:      fields.Labels,
		ParentKey:   fields.ParentKey,
		RawFields:   rawFields,
	})
	if err != nil {
		return client.CreatedIssue{}, err
	}
	return client.CreatedIssue{
		Key:  resp.GetKey(),
		ID:   resp.GetId(),
		Self: resp.GetSelf(),
	}, nil
}

func (b *bridgeBackend) UpdateIssue(ctx context.Context, key string, fields UpdateIssueFields) error {
	rawFields, err := rawFieldsToProto(fields.RawFields)
	if err != nil {
		return err
	}
	// UpdateIssueRequest proto carries summary/description/raw_fields
	// only. Bridge-mode labels and assignee updates are routed
	// through raw_fields using their Jira system-field IDs ("labels"
	// and "assignee"), matching what gojira-client does. The facade
	// path uses typed options directly.
	if rawFields == nil && (len(fields.Labels) > 0 || fields.Assignee != "") {
		rawFields = map[string]string{}
	}
	if len(fields.Labels) > 0 {
		labelsJSON, _ := json.Marshal(fields.Labels)
		rawFields["labels"] = string(labelsJSON)
	}
	if fields.Assignee != "" {
		assigneeJSON, _ := json.Marshal(map[string]string{"accountId": fields.Assignee})
		rawFields["assignee"] = string(assigneeJSON)
	}
	_, err = b.client.UpdateIssue(ctx, &gojirav1.UpdateIssueRequest{
		Key:         key,
		Summary:     fields.Summary,
		Description: fields.Description,
		RawFields:   rawFields,
	})
	return err
}

func (b *bridgeBackend) AddComment(ctx context.Context, key, text string) (client.Comment, error) {
	resp, err := b.client.AddComment(ctx, &gojirav1.AddCommentRequest{
		Key:      key,
		BodyText: text,
	})
	if err != nil {
		return client.Comment{}, err
	}
	return client.Comment{
		ID:                resp.GetId(),
		AuthorDisplayName: resp.GetAuthorDisplayName(),
		Created:           resp.GetCreated(),
	}, nil
}

func (b *bridgeBackend) ListTransitions(ctx context.Context, key string) ([]client.Transition, error) {
	resp, err := b.client.ListTransitions(ctx, &gojirav1.ListTransitionsRequest{Key: key})
	if err != nil {
		return nil, err
	}
	out := make([]client.Transition, 0, len(resp.GetTransitions()))
	for _, t := range resp.GetTransitions() {
		out = append(out, client.Transition{
			ID:       t.GetId(),
			Name:     t.GetName(),
			ToStatus: t.GetToStatus(),
		})
	}
	return out, nil
}

func (b *bridgeBackend) TransitionIssue(ctx context.Context, key, transitionID, toStatus string, fields TransitionFields) error {
	switch {
	case transitionID != "" && toStatus != "":
		return errors.New("transition_issue: pass exactly one of transition_id or to_status, not both")
	case transitionID == "" && toStatus == "":
		return errors.New("transition_issue: pass exactly one of transition_id or to_status")
	}
	_, err := b.client.TransitionIssue(ctx, &gojirav1.TransitionIssueRequest{
		Key:              key,
		TransitionId:     transitionID,
		TargetStatusName: toStatus,
		CommentText:      fields.CommentText,
	})
	return err
}

// ---------------------------------------------------------------------------
// helpers — kind-string conversions and raw-fields marshalling
// ---------------------------------------------------------------------------

// classifyKindFromString reverses [classify.Kind.String]. Returns
// classify.KindExternal as the safe default when the input doesn't
// match a known kind.
func classifyKindFromString(s string) (classify.Kind, bool) {
	switch s {
	case "JiraKey":
		return classify.KindJiraKey, true
	case "JiraURL":
		return classify.KindJiraURL, true
	case "GitHubPR":
		return classify.KindGitHubPR, true
	case "External":
		return classify.KindExternal, true
	default:
		return classify.KindExternal, false
	}
}

// rawFieldsToProto converts the map[string]any the MCP tool collects
// into the map[string]string proto field shape (raw_fields). Values
// are JSON-encoded so structured Jira fields survive the trip.
func rawFieldsToProto(in map[string]any) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		// String values pass through as-is (Jira's raw-fields contract
		// is "JSON value", and a quoted string is valid JSON; sending
		// it unquoted matches what `gojira-client` does).
		if s, ok := v.(string); ok {
			out[k] = s
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("raw_fields[%s]: %w", k, err)
		}
		out[k] = string(b)
	}
	return out, nil
}
