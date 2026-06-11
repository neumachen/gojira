// White-box tests for the Phase-2 write-operation gRPC handlers
// (CreateIssue, UpdateIssue, AddComment, ListTransitions,
// TransitionIssue). They live in package grpc so they can
// overwrite the unexported function-field seams with in-process fakes,
// driving the real handler code without any network calls.
package grpc

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/pkg/client"
)

// writeServer builds a Server with the supplied write-fake injections.
// Each parameter may be nil to keep the production default — but in
// practice every test below supplies the fake it cares about.
func writeServer(t *testing.T, opts ...Option) *Server {
	t.Helper()
	return NewServer(gojira.Config{
		Site:      "https://example.atlassian.net",
		OutputDir: t.TempDir(),
	}, opts...)
}

// ---------------------------------------------------------------------------
// CreateIssue
// ---------------------------------------------------------------------------

func TestServer_CreateIssue_Success(t *testing.T) {
	t.Parallel()
	var gotProject, gotIssueType string
	srv := writeServer(t, WithCreateIssueFunc(
		func(_ context.Context, _ gojira.Config, project, issueType string, _ ...client.CreateOption) (client.CreatedIssue, error) {
			gotProject = project
			gotIssueType = issueType
			return client.CreatedIssue{Key: "PROJ-1", ID: "10001", Self: "u"}, nil
		},
	))

	resp, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project:   "PROJ",
		IssueType: "Task",
		Summary:   "S",
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if resp.GetKey() != "PROJ-1" || resp.GetId() != "10001" || resp.GetSelf() != "u" {
		t.Errorf("response mismatch: %+v", resp)
	}
	if gotProject != "PROJ" || gotIssueType != "Task" {
		t.Errorf("fake fn received project=%q issueType=%q", gotProject, gotIssueType)
	}
	if len(resp.GetDryRunBody()) != 0 {
		t.Errorf("DryRunBody must be empty on a real create, got %d bytes", len(resp.GetDryRunBody()))
	}
}

func TestServer_CreateIssue_DryRunSkipsCall(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := writeServer(t, WithCreateIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
			called.Store(true)
			return client.CreatedIssue{}, nil
		},
	))

	resp, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project:   "PROJ",
		IssueType: "Task",
		Summary:   "S",
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if called.Load() {
		t.Error("createIssueFn must NOT be called when DryRun is set")
	}
	if len(resp.GetDryRunBody()) == 0 {
		t.Error("DryRunBody must be populated on a dry-run create")
	}
	if resp.GetKey() != "" || resp.GetId() != "" {
		t.Errorf("dry-run response must not carry key/id, got %+v", resp)
	}
}

func TestServer_CreateIssue_MissingProject(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithCreateIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
			t.Fatal("createIssueFn must not be called when validation fails")
			return client.CreatedIssue{}, nil
		},
	))
	_, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{IssueType: "Task"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

func TestServer_CreateIssue_MissingIssueType(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithCreateIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
			t.Fatal("createIssueFn must not be called")
			return client.CreatedIssue{}, nil
		},
	))
	_, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{Project: "PROJ"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

func TestServer_CreateIssue_ErrorMapsToStatus(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithCreateIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.CreateOption) (client.CreatedIssue, error) {
			return client.CreatedIssue{}, client.ErrBadRequest
		},
	))
	_, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project: "PROJ", IssueType: "Task",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("ErrBadRequest must map to InvalidArgument; got %v", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateIssue
// ---------------------------------------------------------------------------

func TestServer_UpdateIssue_Success(t *testing.T) {
	t.Parallel()
	var gotKey string
	srv := writeServer(t, WithUpdateIssueFunc(
		func(_ context.Context, _ gojira.Config, key string, _ ...client.UpdateOption) error {
			gotKey = key
			return nil
		},
	))
	resp, err := srv.UpdateIssue(context.Background(), &gojirav1.UpdateIssueRequest{
		Key:     "PROJ-1",
		Summary: "new",
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on a real update")
	}
	if gotKey != "PROJ-1" {
		t.Errorf("fake got key=%q, want PROJ-1", gotKey)
	}
}

func TestServer_UpdateIssue_DryRunSkipsCall(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := writeServer(t, WithUpdateIssueFunc(
		func(context.Context, gojira.Config, string, ...client.UpdateOption) error {
			called.Store(true)
			return nil
		},
	))
	resp, err := srv.UpdateIssue(context.Background(), &gojirav1.UpdateIssueRequest{
		Key:     "PROJ-1",
		Summary: "new",
		DryRun:  true,
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if called.Load() {
		t.Error("updateIssueFn must NOT be called when DryRun is set")
	}
	if resp.GetOk() {
		t.Error("Ok must be false on dry-run (no change made)")
	}
	if len(resp.GetDryRunBody()) == 0 {
		t.Error("DryRunBody must be populated")
	}
}

func TestServer_UpdateIssue_MissingKey(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithUpdateIssueFunc(
		func(context.Context, gojira.Config, string, ...client.UpdateOption) error {
			t.Fatal("updateIssueFn must not be called when validation fails")
			return nil
		},
	))
	_, err := srv.UpdateIssue(context.Background(), &gojirav1.UpdateIssueRequest{Summary: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

// ---------------------------------------------------------------------------
// AddComment
// ---------------------------------------------------------------------------

func TestServer_AddComment_Success(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithAddCommentFunc(
		func(context.Context, gojira.Config, string, ...client.CommentOption) (client.Comment, error) {
			return client.Comment{
				ID:                "10100",
				AuthorDisplayName: "Alice",
				Created:           "2026-01-15T10:30:00.000+0000",
			}, nil
		},
	))
	resp, err := srv.AddComment(context.Background(), &gojirav1.AddCommentRequest{
		Key:      "PROJ-1",
		BodyText: "ok",
	})
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if resp.GetId() != "10100" {
		t.Errorf("Id: got %q", resp.GetId())
	}
	if resp.GetAuthorDisplayName() != "Alice" {
		t.Errorf("AuthorDisplayName: got %q", resp.GetAuthorDisplayName())
	}
	if resp.GetCreated() != "2026-01-15T10:30:00.000+0000" {
		t.Errorf("Created: got %q", resp.GetCreated())
	}
}

func TestServer_AddComment_MissingKey(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithAddCommentFunc(
		func(context.Context, gojira.Config, string, ...client.CommentOption) (client.Comment, error) {
			t.Fatal("addCommentFn must not be called when validation fails")
			return client.Comment{}, nil
		},
	))
	_, err := srv.AddComment(context.Background(), &gojirav1.AddCommentRequest{BodyText: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

// ---------------------------------------------------------------------------
// ListTransitions
// ---------------------------------------------------------------------------

func TestServer_ListTransitions_Success(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithListTransitionsFunc(
		func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{
				{ID: "11", Name: "Start", ToStatus: "In Progress"},
				{ID: "21", Name: "Done", ToStatus: "Done"},
			}, nil
		},
	))
	resp, err := srv.ListTransitions(context.Background(), &gojirav1.ListTransitionsRequest{Key: "PROJ-1"})
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	got := resp.GetTransitions()
	if len(got) != 2 {
		t.Fatalf("want 2 transitions, got %d", len(got))
	}
	if got[0].GetId() != "11" || got[0].GetName() != "Start" || got[0].GetToStatus() != "In Progress" {
		t.Errorf("transition[0]: %+v", got[0])
	}
	if got[1].GetId() != "21" || got[1].GetToStatus() != "Done" {
		t.Errorf("transition[1]: %+v", got[1])
	}
}

func TestServer_ListTransitions_MissingKey(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithListTransitionsFunc(
		func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			t.Fatal("listTransitionsFn must not be called")
			return nil, nil
		},
	))
	_, err := srv.ListTransitions(context.Background(), &gojirav1.ListTransitionsRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

// ---------------------------------------------------------------------------
// TransitionIssue (by id, by status name, error paths)
// ---------------------------------------------------------------------------

func TestServer_TransitionIssue_ByID(t *testing.T) {
	t.Parallel()
	var gotKey, gotID string
	srv := writeServer(t, WithTransitionIssueFunc(
		func(_ context.Context, _ gojira.Config, key, transitionID string, _ ...client.TransitionOption) error {
			gotKey = key
			gotID = transitionID
			return nil
		},
	))
	resp, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:          "PROJ-1",
		TransitionId: "11",
	})
	if err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on success")
	}
	if gotKey != "PROJ-1" || gotID != "11" {
		t.Errorf("fake got key=%q id=%q", gotKey, gotID)
	}
}

func TestServer_TransitionIssue_ByStatusResolves(t *testing.T) {
	t.Parallel()
	var transitionedWith string
	srv := writeServer(t,
		WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{
				{ID: "11", ToStatus: "In Progress"},
				{ID: "21", ToStatus: "Done"},
			}, nil
		}),
		WithTransitionIssueFunc(func(_ context.Context, _ gojira.Config, _, transitionID string, _ ...client.TransitionOption) error {
			transitionedWith = transitionID
			return nil
		}),
	)
	resp, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:              "PROJ-1",
		TargetStatusName: "done", // case-insensitive
	})
	if err != nil {
		t.Fatalf("TransitionIssue by status: %v", err)
	}
	if !resp.GetOk() {
		t.Error("Ok must be true on success")
	}
	if transitionedWith != "21" {
		t.Errorf("by-status must resolve to id 21, got %q", transitionedWith)
	}
}

func TestServer_TransitionIssue_ByStatusNoMatch(t *testing.T) {
	t.Parallel()
	srv := writeServer(t,
		WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{{ID: "11", ToStatus: "In Progress"}}, nil
		}),
		WithTransitionIssueFunc(func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
			t.Fatal("transitionIssueFn must not be called when resolution fails")
			return nil
		}),
	)
	_, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:              "PROJ-1",
		TargetStatusName: "Resolved",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.NotFound {
		t.Errorf("no-match must map to NotFound; got %v", got)
	}
}

func TestServer_TransitionIssue_ByStatusAmbiguous(t *testing.T) {
	t.Parallel()
	srv := writeServer(t,
		WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			return []client.Transition{
				{ID: "31", ToStatus: "Resolved"},
				{ID: "32", ToStatus: "Resolved"},
			}, nil
		}),
		WithTransitionIssueFunc(func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
			t.Fatal("transitionIssueFn must not be called when resolution is ambiguous")
			return nil
		}),
	)
	_, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:              "PROJ-1",
		TargetStatusName: "Resolved",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.FailedPrecondition {
		t.Errorf("ambiguous must map to FailedPrecondition; got %v", got)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
		t.Errorf("status message must mention ambiguity; got %v", err)
	}
}

func TestServer_TransitionIssue_NeitherIDNorName(t *testing.T) {
	t.Parallel()
	srv := writeServer(t,
		WithListTransitionsFunc(func(context.Context, gojira.Config, string) ([]client.Transition, error) {
			t.Fatal("listTransitionsFn must not be called")
			return nil, nil
		}),
		WithTransitionIssueFunc(func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
			t.Fatal("transitionIssueFn must not be called")
			return nil
		}),
	)
	_, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{Key: "PROJ-1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("missing id+name must map to InvalidArgument; got %v", got)
	}
}

func TestServer_TransitionIssue_ConflictMapsToFailedPrecondition(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithTransitionIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
			return client.ErrConflict
		},
	))
	_, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{
		Key:          "PROJ-1",
		TransitionId: "11",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.FailedPrecondition {
		t.Errorf("ErrConflict must map to FailedPrecondition; got %v", got)
	}
}

// TestServer_TransitionIssue_MissingKey is a focused validation guard so
// the key-required precondition is observed even when an id or name is
// supplied.
func TestServer_TransitionIssue_MissingKey(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithTransitionIssueFunc(
		func(context.Context, gojira.Config, string, string, ...client.TransitionOption) error {
			t.Fatal("transitionIssueFn must not be called when key is empty")
			return nil
		}),
	)
	_, err := srv.TransitionIssue(context.Background(), &gojirav1.TransitionIssueRequest{TransitionId: "11"})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := codeOf(err); got != codes.InvalidArgument {
		t.Errorf("status code: got %v, want InvalidArgument", got)
	}
}

// Quick assertion that the new injectable seams are still wired by
// the production default — invoking the production NewServer with no
// options must NOT panic and the function-field seams must be non-nil.
// (Smoke-test only; behaviour of the production closures is exercised
// elsewhere by the integration test in Group 5.)
func TestServer_WriteSeams_HaveProductionDefaults(t *testing.T) {
	t.Parallel()
	s := NewServer(gojira.Config{})

	if s.createIssueFn == nil {
		t.Error("createIssueFn must default to a non-nil closure")
	}
	if s.updateIssueFn == nil {
		t.Error("updateIssueFn must default to a non-nil closure")
	}
	if s.addCommentFn == nil {
		t.Error("addCommentFn must default to a non-nil closure")
	}
	if s.listTransitionsFn == nil {
		t.Error("listTransitionsFn must default to a non-nil closure")
	}
	if s.transitionIssueFn == nil {
		t.Error("transitionIssueFn must default to a non-nil closure")
	}

	// Silence unused-import linters in the rare case a future
	// refactor drops every other reference to errors.
	_ = errors.New
}
