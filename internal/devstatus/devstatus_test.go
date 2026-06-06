package devstatus

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakeDevStatusClient (per-(app,dataType) call tracking)
// ---------------------------------------------------------------------------

// fakeDevStatusClient is a minimal devStatusClient implementation that
// tracks every (application, dataType) pair the Enricher requests so
// tests can assert that the fan-out covered every configured pair.
type fakeDevStatusClient struct {
	mu        sync.Mutex
	responses map[string]map[string]client.DevStatusResponse
	errs      map[string]map[string]error
	calls     atomic.Int64
	perCall   map[string]*atomic.Int64 // key: "<app>/<dataType>"
}

func newFakeDevStatusClient() *fakeDevStatusClient {
	return &fakeDevStatusClient{
		responses: make(map[string]map[string]client.DevStatusResponse),
		errs:      make(map[string]map[string]error),
		perCall:   make(map[string]*atomic.Int64),
	}
}

func (f *fakeDevStatusClient) setResponse(app, dt string, resp client.DevStatusResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.responses[app]; !ok {
		f.responses[app] = make(map[string]client.DevStatusResponse)
	}
	f.responses[app][dt] = resp
}

func (f *fakeDevStatusClient) setError(app, dt string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.errs[app]; !ok {
		f.errs[app] = make(map[string]error)
	}
	f.errs[app][dt] = err
}

// count returns the number of times the fake was called for the given
// (app, dataType) pair.
func (f *fakeDevStatusClient) count(app, dt string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.perCall[app+"/"+dt]
	if !ok {
		return 0
	}
	return c.Load()
}

// dataTypesCalled returns the sorted set of dataType values the fake
// was called with, deduplicated across applications.
func (f *fakeDevStatusClient) dataTypesCalled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := make(map[string]bool)
	for key, c := range f.perCall {
		if c.Load() == 0 {
			continue
		}
		// key is "<app>/<dataType>".
		for i := 0; i < len(key); i++ {
			if key[i] == '/' {
				seen[key[i+1:]] = true
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for dt := range seen {
		out = append(out, dt)
	}
	sort.Strings(out)
	return out
}

func (f *fakeDevStatusClient) DevStatus(_ context.Context, _, application, dataType string) (client.DevStatusResponse, error) {
	f.calls.Add(1)
	key := application + "/" + dataType
	f.mu.Lock()
	c, ok := f.perCall[key]
	if !ok {
		c = &atomic.Int64{}
		f.perCall[key] = c
	}
	f.mu.Unlock()
	c.Add(1)

	f.mu.Lock()
	if errs, ok := f.errs[application]; ok {
		if err, ok := errs[dataType]; ok {
			f.mu.Unlock()
			return client.DevStatusResponse{}, err
		}
	}
	if resps, ok := f.responses[application]; ok {
		if resp, ok := resps[dataType]; ok {
			f.mu.Unlock()
			return resp, nil
		}
	}
	f.mu.Unlock()
	return client.DevStatusResponse{}, nil
}

// newTestEnricher wires the fake client into an *Enricher without
// going through the real client.New.
func newTestEnricher(fc *fakeDevStatusClient, cfg config.Config) *Enricher {
	return &Enricher{client: fc, cfg: cfg}
}

// prResponse builds a single-instance DevStatusResponse with one PR.
func prResponse(url, title, status, repo string) client.DevStatusResponse {
	return client.DevStatusResponse{
		Detail: []client.DevStatusInstance{{
			Instance: client.DevStatusInstanceMeta{Type: "GitHub", Name: "GitHub"},
			PullRequests: []client.DevStatusPR{{
				ID:         "#1",
				URL:        url,
				Name:       title,
				Status:     status,
				Repository: repo,
				LastUpdate: "2026-05-08T13:44:52.000+0000",
				Author:     client.DevStatusPerson{Name: "Alice"},
			}},
		}},
	}
}

func baseIssue(numericID string) parse.Issue {
	return parse.Issue{
		Key:       "PLATENG-1573",
		NumericID: numericID,
	}
}

func allDataTypes() []string {
	return []string{"pullrequest", "branch", "commit", "repository", "build"}
}

// alphabeticalDataTypes is the canonical-set sorted alphabetically;
// used to compare against fakeDevStatusClient.dataTypesCalled which
// returns its keys in alphabetical order for deterministic test output.
func alphabeticalDataTypes() []string {
	out := append([]string(nil), allDataTypes()...)
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Enricher behaviour
// ---------------------------------------------------------------------------

func TestEnricher_DefaultOff(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setResponse("GitHub", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T", "MERGED", "o/r"))

	cfg := config.Config{
		IncludeDevStatus:      false, // OFF
		DevStatusApplications: []string{"GitHub"},
		DevStatusDataTypes:    allDataTypes(),
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	assert.Equal(t, parse.DevStatusData{}, got, "no entities when IncludeDevStatus is false")
	assert.Equal(t, int64(0), fc.calls.Load(), "no DevStatus calls expected")
}

// TestEnricher_AlwaysCallsAllDataTypes locks in the post-gate-removal
// contract: whenever IncludeDevStatus=true and a numeric ID is
// available, every configured (application, dataType) pair is queried,
// regardless of what (if anything) the customfield_10000 summary blob
// reports. This is the explicit reversal of the smart-gate behaviour
// that produced two silent-miss bugs (PLATENG-1578 in particular).
//
// The cases enumerate the four classes of summary state the old gate
// branched on; the new contract collapses them all to "query every
// configured dataType".
func TestEnricher_AlwaysCallsAllDataTypes(t *testing.T) {
	// Every case below uses the same fake client with one PR-shaped
	// response for dataType=pullrequest, so we can also verify the
	// successful call still populates the result while the other four
	// dataTypes return empty.
	cases := []struct {
		name string
	}{
		{name: "no customfield_10000 set on issue"},
		{name: "summary present and parseable (legacy gate would have steered)"},
		{name: "summary present but unparseable"},
		{name: "summary present and reports zero everywhere (legacy stale path)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeDevStatusClient()
			fc.setResponse("GitHub", "pullrequest",
				prResponse("https://github.com/o/r/pull/1", "T", "MERGED", "o/r"))

			cfg := config.Config{
				IncludeDevStatus:      true,
				DevStatusApplications: []string{"GitHub"},
				DevStatusDataTypes:    allDataTypes(),
			}
			e := newTestEnricher(fc, cfg)

			// The issue carries no customfield_10000. The new Enrich
			// does not even look at it; the case name documents what
			// the legacy gate would have branched on.
			issue := baseIssue("86679")

			got, err := e.Enrich(context.Background(), issue)
			require.NoError(t, err)
			assert.Equal(t, int64(5), fc.calls.Load(),
				"all five configured dataTypes queried; gate is gone")
			assert.Equal(t, alphabeticalDataTypes(), fc.dataTypesCalled(),
				"every canonical dataType is in the called set")
			require.Len(t, got.PullRequests, 1, "PR result preserved")
		})
	}
}

func TestEnricher_NilWhenNumericIDEmpty(t *testing.T) {
	fc := newFakeDevStatusClient()
	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub"},
		DevStatusDataTypes:    allDataTypes(),
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue(""))
	require.NoError(t, err)
	assert.Equal(t, parse.DevStatusData{}, got)
	assert.Equal(t, int64(0), fc.calls.Load(), "no NumericID → no call")
}

func TestEnricher_MergesAcrossApplications(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setResponse("GitHub", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T1", "MERGED", "o/r"))
	fc.setResponse("Bitbucket", "pullrequest",
		prResponse("https://bitbucket.org/o/r/pull-requests/2", "T2", "OPEN", "o/r"))

	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub", "Bitbucket"},
		// Limit to one dataType to focus the assertion on the per-app
		// fan-out.
		DevStatusDataTypes: []string{"pullrequest"},
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	require.Len(t, got.PullRequests, 2)
	// Sorted by URL: bitbucket.org < github.com.
	assert.Equal(t, "https://bitbucket.org/o/r/pull-requests/2", got.PullRequests[0].URL)
	assert.Equal(t, "Bitbucket", got.PullRequests[0].Application)
	assert.Equal(t, "https://github.com/o/r/pull/1", got.PullRequests[1].URL)
	assert.Equal(t, "GitHub", got.PullRequests[1].Application)
}

func TestEnricher_DedupsAcrossApplications(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setResponse("GitHub", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T1", "MERGED", "o/r"))
	fc.setResponse("Bitbucket", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T1", "MERGED", "o/r"))

	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub", "Bitbucket"},
		DevStatusDataTypes:    []string{"pullrequest"},
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	require.Len(t, got.PullRequests, 1, "duplicate URL must collapse")
	assert.Equal(t, "GitHub", got.PullRequests[0].Application)
}

func TestEnricher_PartialFailureReturnsDataPlusError(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setResponse("GitHub", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T1", "MERGED", "o/r"))
	fc.setError("Bitbucket", "pullrequest", client.ErrForbidden)

	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub", "Bitbucket"},
		DevStatusDataTypes:    []string{"pullrequest"},
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.Error(t, err, "partial failure must surface a non-fatal error")
	assert.True(t, errors.Is(err, client.ErrForbidden), "wrapped sentinel preserved")
	require.Len(t, got.PullRequests, 1)
	assert.Equal(t, "GitHub", got.PullRequests[0].Application)
}

func TestEnricher_AllFail(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setError("GitHub", "pullrequest", client.ErrNotFound)
	fc.setError("Bitbucket", "pullrequest", client.ErrForbidden)

	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub", "Bitbucket"},
		DevStatusDataTypes:    []string{"pullrequest"},
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.Error(t, err, "total failure must surface a fatal error")
	assert.Equal(t, parse.DevStatusData{}, got, "no entities collected")
	assert.True(t, errors.Is(err, client.ErrNotFound) || errors.Is(err, client.ErrForbidden),
		"at least one underlying sentinel preserved")
}

func TestEnricher_LastUpdateParsed(t *testing.T) {
	fc := newFakeDevStatusClient()
	fc.setResponse("GitHub", "pullrequest",
		prResponse("https://github.com/o/r/pull/1", "T", "MERGED", "o/r"))

	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub"},
		DevStatusDataTypes:    []string{"pullrequest"},
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	require.Len(t, got.PullRequests, 1)
	assert.False(t, got.PullRequests[0].LastUpdate.IsZero(), "ISO-8601 with milliseconds must parse")
	assert.Equal(t, 2026, got.PullRequests[0].LastUpdate.Year())
}

// TestEnricher_EmptyDataTypesShortCircuits asserts that an explicit
// empty DevStatusDataTypes list disables enrichment without calling
// the Dev Status endpoint at all.
func TestEnricher_EmptyDataTypesShortCircuits(t *testing.T) {
	fc := newFakeDevStatusClient()
	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub"},
		DevStatusDataTypes:    nil,
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	assert.Equal(t, parse.DevStatusData{}, got)
	assert.Equal(t, int64(0), fc.calls.Load(), "empty DevStatusDataTypes → no call")
}

// TestEnricher_EmptyApplicationsShortCircuits asserts that an empty
// DevStatusApplications list disables enrichment without calling the
// Dev Status endpoint, mirroring the empty-DataTypes path.
func TestEnricher_EmptyApplicationsShortCircuits(t *testing.T) {
	fc := newFakeDevStatusClient()
	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: nil,
		DevStatusDataTypes:    allDataTypes(),
	}
	e := newTestEnricher(fc, cfg)

	got, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	assert.Equal(t, parse.DevStatusData{}, got)
	assert.Equal(t, int64(0), fc.calls.Load(), "empty DevStatusApplications → no call")
}

// TestEnricher_OrderedDataTypes locks in the canonical-order
// invariant: even if the configured list is in a different order, the
// fan-out per application proceeds in CanonicalDataTypes order. The
// test asserts call counts per (app, dataType) match the configured
// set; ordering itself is exercised through deterministic merge
// behaviour in TestEnricher_MergesAcrossApplications.
func TestEnricher_OrderedDataTypes(t *testing.T) {
	fc := newFakeDevStatusClient()
	cfg := config.Config{
		IncludeDevStatus:      true,
		DevStatusApplications: []string{"GitHub"},
		// Intentional non-canonical order; should not change the result.
		DevStatusDataTypes: []string{"build", "pullrequest", "branch", "commit", "repository"},
	}
	e := newTestEnricher(fc, cfg)

	_, err := e.Enrich(context.Background(), baseIssue("86679"))
	require.NoError(t, err)
	assert.Equal(t, int64(5), fc.calls.Load())
	for _, dt := range allDataTypes() {
		assert.Equal(t, int64(1), fc.count("GitHub", dt),
			"dataType %s called exactly once", dt)
	}
}
