// White-box tests for the version-management gRPC handlers
// (CreateVersion, UpdateVersion, ListVersions) plus the Gap-B
// fixVersions wiring on CreateIssue/UpdateIssue. They live in package
// grpc so they can overwrite the unexported seams with in-process
// fakes — no network.
package grpc

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	gojira "github.com/neumachen/gojira"
	gojirav1 "github.com/neumachen/gojira/gen/gojira/v1"
	"github.com/neumachen/gojira/pkg/client"
)

// ---------------------------------------------------------------------------
// CreateVersion
// ---------------------------------------------------------------------------

func TestServer_CreateVersion_Success(t *testing.T) {
	t.Parallel()
	var gotName, gotProject string
	srv := writeServer(t, WithCreateVersionFunc(
		func(_ context.Context, _ gojira.Config, name, project string, _ ...client.VersionOption) (client.Version, error) {
			gotName, gotProject = name, project
			return client.Version{ID: "20000", Name: name, ProjectID: 10000, Released: true}, nil
		},
	))

	resp, err := srv.CreateVersion(context.Background(), &gojirav1.CreateVersionRequest{
		Name:     "Release-abc1234",
		Project:  "10000",
		Released: true,
	})
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if gotName != "Release-abc1234" || gotProject != "10000" {
		t.Errorf("seam received name=%q project=%q", gotName, gotProject)
	}
	v := resp.GetVersion()
	if v.GetId() != "20000" || v.GetProjectId() != 10000 || !v.GetReleased() {
		t.Errorf("mapped version mismatch: %+v", v)
	}
	if len(resp.GetDryRunBody()) != 0 {
		t.Errorf("DryRunBody must be empty on a real create")
	}
}

func TestServer_CreateVersion_DryRunSkipsCall(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := writeServer(t, WithCreateVersionFunc(
		func(context.Context, gojira.Config, string, string, ...client.VersionOption) (client.Version, error) {
			called.Store(true)
			return client.Version{}, nil
		},
	))

	resp, err := srv.CreateVersion(context.Background(), &gojirav1.CreateVersionRequest{
		Name:        "Release-abc1234",
		Project:     "10000",
		Released:    true,
		ReleaseDate: "2000-01-01",
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if called.Load() {
		t.Error("createVersionFn must NOT be called when DryRun is set")
	}
	var body map[string]any
	if err := json.Unmarshal(resp.GetDryRunBody(), &body); err != nil {
		t.Fatalf("decode dry-run body: %v", err)
	}
	if body["name"] != "Release-abc1234" {
		t.Errorf("dry-run body name=%v", body["name"])
	}
	if body["projectId"] != float64(10000) {
		t.Errorf("numeric project must emit projectId number, got %v", body["projectId"])
	}
	if resp.GetVersion() != nil {
		t.Error("dry-run response must not carry a version")
	}
}

func TestServer_CreateVersion_Validation(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	if _, err := srv.CreateVersion(context.Background(), &gojirav1.CreateVersionRequest{Project: "10000"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("missing name: got %v", codeOf(err))
	}
	if _, err := srv.CreateVersion(context.Background(), &gojirav1.CreateVersionRequest{Name: "x"}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("missing project: got %v", codeOf(err))
	}
}

func TestServer_CreateVersion_ErrorMapsToStatus(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithCreateVersionFunc(
		func(context.Context, gojira.Config, string, string, ...client.VersionOption) (client.Version, error) {
			return client.Version{}, client.ErrBadRequest
		},
	))
	_, err := srv.CreateVersion(context.Background(), &gojirav1.CreateVersionRequest{Name: "x", Project: "10000"})
	if codeOf(err) != codes.InvalidArgument {
		t.Errorf("ErrBadRequest must map to InvalidArgument; got %v", codeOf(err))
	}
}

// ---------------------------------------------------------------------------
// UpdateVersion
// ---------------------------------------------------------------------------

func TestServer_UpdateVersion_Success(t *testing.T) {
	t.Parallel()
	var gotID string
	srv := writeServer(t, WithUpdateVersionFunc(
		func(_ context.Context, _ gojira.Config, id string, _ ...client.VersionOption) (client.Version, error) {
			gotID = id
			return client.Version{ID: id, Released: true}, nil
		},
	))
	resp, err := srv.UpdateVersion(context.Background(), &gojirav1.UpdateVersionRequest{Id: "20000", Released: proto.Bool(true)})
	if err != nil {
		t.Fatalf("UpdateVersion: %v", err)
	}
	if gotID != "20000" || resp.GetVersion().GetId() != "20000" {
		t.Errorf("mismatch: gotID=%q resp=%+v", gotID, resp.GetVersion())
	}
}

func TestServer_UpdateVersion_DryRunSkipsCall(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := writeServer(t, WithUpdateVersionFunc(
		func(context.Context, gojira.Config, string, ...client.VersionOption) (client.Version, error) {
			called.Store(true)
			return client.Version{}, nil
		},
	))
	resp, err := srv.UpdateVersion(context.Background(), &gojirav1.UpdateVersionRequest{Id: "20000", Released: proto.Bool(true), DryRun: true})
	if err != nil {
		t.Fatalf("UpdateVersion: %v", err)
	}
	if called.Load() {
		t.Error("updateVersionFn must NOT be called when DryRun is set")
	}
	var body map[string]any
	if err := json.Unmarshal(resp.GetDryRunBody(), &body); err != nil {
		t.Fatalf("decode dry-run body: %v", err)
	}
	if body["released"] != true {
		t.Errorf("dry-run body released=%v", body["released"])
	}
}

func TestServer_UpdateVersion_MissingID(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	if _, err := srv.UpdateVersion(context.Background(), &gojirav1.UpdateVersionRequest{Released: proto.Bool(true)}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("missing id: got %v", codeOf(err))
	}
}

// TestServer_UpdateVersion_OmittedBoolsNotApplied pins the presence
// semantics: with released/archived unset (nil *bool), the built body must
// carry NEITHER key, so an update of only the description leaves the
// existing Jira released/archived values untouched. Observed via the
// network-free dry-run body.
func TestServer_UpdateVersion_OmittedBoolsNotApplied(t *testing.T) {
	t.Parallel()
	var called atomic.Bool
	srv := writeServer(t, WithUpdateVersionFunc(
		func(context.Context, gojira.Config, string, ...client.VersionOption) (client.Version, error) {
			called.Store(true)
			return client.Version{}, nil
		},
	))
	resp, err := srv.UpdateVersion(context.Background(), &gojirav1.UpdateVersionRequest{
		Id:          "20000",
		Description: "just the description",
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("UpdateVersion: %v", err)
	}
	if called.Load() {
		t.Error("updateVersionFn must NOT be called when DryRun is set")
	}
	var body map[string]any
	if err := json.Unmarshal(resp.GetDryRunBody(), &body); err != nil {
		t.Fatalf("decode dry-run body: %v", err)
	}
	if _, ok := body["released"]; ok {
		t.Errorf("released must be ABSENT when unset; body=%v", body)
	}
	if _, ok := body["archived"]; ok {
		t.Errorf("archived must be ABSENT when unset; body=%v", body)
	}
	if body["description"] != "just the description" {
		t.Errorf("description should still be present; body=%v", body)
	}
}

// TestServer_UpdateVersion_PresentBoolsApplied is the complement: an
// explicit released=true and archived=false (both present via proto.Bool)
// must both appear in the built body — including the explicit false, which
// is distinguishable from unset only because the field carries presence.
func TestServer_UpdateVersion_PresentBoolsApplied(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	resp, err := srv.UpdateVersion(context.Background(), &gojirav1.UpdateVersionRequest{
		Id:       "20000",
		Released: proto.Bool(true),
		Archived: proto.Bool(false),
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("UpdateVersion: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(resp.GetDryRunBody(), &body); err != nil {
		t.Fatalf("decode dry-run body: %v", err)
	}
	if body["released"] != true {
		t.Errorf("released must be present and true; body=%v", body)
	}
	got, ok := body["archived"]
	if !ok || got != false {
		t.Errorf("archived must be present and false (explicit); body=%v", body)
	}
}

// ---------------------------------------------------------------------------
// ListVersions
// ---------------------------------------------------------------------------

func TestServer_ListVersions_Success(t *testing.T) {
	t.Parallel()
	srv := writeServer(t, WithListVersionsFunc(
		func(_ context.Context, _ gojira.Config, projectIDOrKey string) ([]client.Version, error) {
			return []client.Version{
				{ID: "20000", Name: "1.0", Released: true, ReleaseDate: "2000-01-01"},
				{ID: "20001", Name: "2.0"},
			}, nil
		},
	))
	resp, err := srv.ListVersions(context.Background(), &gojirav1.ListVersionsRequest{Project: "EXAMPLE"})
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(resp.GetVersions()) != 2 {
		t.Fatalf("want 2 versions, got %d", len(resp.GetVersions()))
	}
	if resp.GetVersions()[0].GetId() != "20000" || !resp.GetVersions()[0].GetReleased() {
		t.Errorf("first version mismatch: %+v", resp.GetVersions()[0])
	}
}

func TestServer_ListVersions_MissingProject(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	if _, err := srv.ListVersions(context.Background(), &gojirav1.ListVersionsRequest{}); codeOf(err) != codes.InvalidArgument {
		t.Errorf("missing project: got %v", codeOf(err))
	}
}

// ---------------------------------------------------------------------------
// Gap B — fixVersions wiring on CreateIssue / UpdateIssue
// ---------------------------------------------------------------------------

func TestServer_CreateIssue_FixVersions_InDryRunBody(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	resp, err := srv.CreateIssue(context.Background(), &gojirav1.CreateIssueRequest{
		Project:       "PROJ",
		IssueType:     "Task",
		FixVersionIds: []string{"10000"},
		DryRun:        true,
	})
	if err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	fields := fieldsFromBody(t, resp.GetDryRunBody())
	fv, ok := fields["fixVersions"].([]any)
	if !ok || len(fv) != 1 {
		t.Fatalf("fixVersions missing/wrong: %#v", fields["fixVersions"])
	}
	if got := fv[0].(map[string]any)["id"]; got != "10000" {
		t.Errorf("fixVersions[0].id = %v", got)
	}
}

func TestServer_UpdateIssue_FixVersions_InDryRunBody(t *testing.T) {
	t.Parallel()
	srv := writeServer(t)
	resp, err := srv.UpdateIssue(context.Background(), &gojirav1.UpdateIssueRequest{
		Key:         "PROJ-1",
		FixVersions: []string{"1.0"},
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	fields := fieldsFromBody(t, resp.GetDryRunBody())
	fv, ok := fields["fixVersions"].([]any)
	if !ok || len(fv) != 1 {
		t.Fatalf("fixVersions missing/wrong: %#v", fields["fixVersions"])
	}
	if got := fv[0].(map[string]any)["name"]; got != "1.0" {
		t.Errorf("fixVersions[0].name = %v", got)
	}
}

// fieldsFromBody decodes a rendered issue body and returns its "fields"
// sub-object.
func fieldsFromBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	f, ok := body["fields"].(map[string]any)
	if !ok {
		t.Fatalf(`"fields" missing from body: %s`, string(raw))
	}
	return f
}
