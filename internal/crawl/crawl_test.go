package crawl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neumachen/gojira/client"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/events"
	"github.com/neumachen/gojira/internal/output"
	"github.com/neumachen/gojira/internal/parse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// fakeFetcher — a minimal fetch.Fetcher implementation for tests.
// ---------------------------------------------------------------------------

// fakeFetcher satisfies fetch.Fetcher. Each key maps to either a raw
// JSON payload or an error. Fetch calls are counted per key.
type fakeFetcher struct {
	mu       sync.Mutex
	payloads map[string][]byte
	errs     map[string]error
	counts   map[string]int
	// delay is an optional sleep applied before returning, used to test
	// concurrency and graceful shutdown.
	delay time.Duration
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		payloads: make(map[string][]byte),
		errs:     make(map[string]error),
		counts:   make(map[string]int),
	}
}

// setPayload registers a raw JSON payload for key.
func (f *fakeFetcher) setPayload(key string, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads[key] = raw
}

// setError registers an error to return for key.
func (f *fakeFetcher) setError(key string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[key] = err
}

// count returns the number of times Fetch was called for key.
func (f *fakeFetcher) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[key]
}

// Fetch implements fetch.Fetcher.
func (f *fakeFetcher) Fetch(ctx context.Context, key string) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.delay):
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[key]++
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	if raw, ok := f.payloads[key]; ok {
		return raw, nil
	}
	return nil, fmt.Errorf("fakeFetcher: no payload or error registered for key %q", key)
}

// ---------------------------------------------------------------------------
// JSON fixture helpers
// ---------------------------------------------------------------------------

// minimalIssueJSON returns a minimal valid Jira issue JSON for key with
// no outbound links.
func minimalIssueJSON(key string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, key, key))
}

// issueWithLinkJSON returns a Jira issue JSON for key that has an
// outward issue link to linkedKey.
func issueWithLinkJSON(key, linkedKey string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [
      {
        "type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
        "outwardIssue": {"key": %q, "fields": {"summary": "Summary of linked"}}
      }
    ],
    "remotelinks": []
  }
}`, key, key, linkedKey))
}

// issueWithPRJSON returns a Jira issue JSON for key that has a remote
// link to a GitHub PR URL.
func issueWithPRJSON(key, prURL string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": [
      {"object": {"title": "PR", "url": %q}}
    ]
  }
}`, key, key, prURL))
}

// ---------------------------------------------------------------------------
// Test config helper
// ---------------------------------------------------------------------------

func testConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Site:        "https://example.atlassian.net",
		User:        "test@example.com",
		Token:       "test-token",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
		IssueCap:    0,
		DepthLimit:  0,
		Refetch:     false,
	}
}

// ---------------------------------------------------------------------------
// countEvents returns the number of events of the given kind in sink.
// ---------------------------------------------------------------------------

func countEvents(sink *events.RecordingSink, kind events.Kind) int {
	n := 0
	for _, e := range sink.Events() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestCrawl_SingleIssue: one start key, no outbound links.
// AC: Summary.Fetched == 1, index.md exists, KindIssueFetched emitted once.
func TestCrawl_SingleIssue(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Fetched, "Fetched")
	assert.Equal(t, 0, sum.Skipped, "Skipped")

	indexPath := filepath.Join(cfg.OutputDir, "EXAMPLE-1", "index.md")
	_, err = os.Stat(indexPath)
	assert.NoError(t, err, "index.md must exist at %s", indexPath)

	assert.Equal(t, 1, countEvents(sink, events.KindIssueFetched), "KindIssueFetched count")
}

// TestCrawl_TwoIssueChain: A links to B; both should be fetched.
// AC: both files exist, KindIssueQueued emitted for both.
func TestCrawl_TwoIssueChain(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setPayload("EXAMPLE-2", minimalIssueJSON("EXAMPLE-2"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Fetched, "Fetched")

	for _, key := range []string{"EXAMPLE-1", "EXAMPLE-2"} {
		p := filepath.Join(cfg.OutputDir, key, "index.md")
		_, err := os.Stat(p)
		assert.NoError(t, err, "index.md must exist for %s", key)
	}

	assert.GreaterOrEqual(t, countEvents(sink, events.KindIssueQueued), 2, "KindIssueQueued count >= 2")
}

// TestCrawl_Deduplication (PRD AC 6): A→B, B→A cycle.
// Each issue must be fetched exactly once; no deadlock.
func TestCrawl_Deduplication(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setPayload("EXAMPLE-2", issueWithLinkJSON("EXAMPLE-2", "EXAMPLE-1"))
	sink := &events.RecordingSink{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Fetched, "Fetched")
	assert.Equal(t, 1, ff.count("EXAMPLE-1"), "EXAMPLE-1 fetch count")
	assert.Equal(t, 1, ff.count("EXAMPLE-2"), "EXAMPLE-2 fetch count")
}

// TestCrawl_IssueCap (PRD AC 7): cap=3, 10 reachable keys.
// Exactly 3 fetched; 7 in CapLimitedKeys; KindIssueCapReached emitted 7 times.
//
// We use a star topology: EXAMPLE-1 links to EXAMPLE-2 through EXAMPLE-10.
// This ensures all 10 keys are discoverable from the first fetch, so the
// cap fires 7 times when EXAMPLE-1 is processed and tries to enqueue all
// 9 children (3 already visited, 7 cap-limited).
func TestCrawl_IssueCap(t *testing.T) {
	cfg := testConfig(t)
	cfg.IssueCap = 3

	// Star topology: EXAMPLE-1 links to EXAMPLE-2 through EXAMPLE-10.
	ff := newFakeFetcher()
	var children []string
	for i := 2; i <= 10; i++ {
		children = append(children, fmt.Sprintf("EXAMPLE-%d", i))
	}
	ff.setPayload("EXAMPLE-1", issueWithMultipleLinksJSON("EXAMPLE-1", children))
	for i := 2; i <= 10; i++ {
		key := fmt.Sprintf("EXAMPLE-%d", i)
		ff.setPayload(key, minimalIssueJSON(key))
	}
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 3, sum.Fetched, "Fetched")
	assert.Equal(t, 7, sum.CapLimited, "CapLimited")
	assert.Len(t, sum.CapLimitedKeys, 7, "CapLimitedKeys")
	assert.Equal(t, 7, countEvents(sink, events.KindIssueCapReached), "KindIssueCapReached count")
}

// TestCrawl_DepthCap (PRD AC 8): chain A→B→C, depthLimit=1.
// A and B fetched; C in CapLimitedKeys.
func TestCrawl_DepthCap(t *testing.T) {
	cfg := testConfig(t)
	cfg.DepthLimit = 1

	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setPayload("EXAMPLE-2", issueWithLinkJSON("EXAMPLE-2", "EXAMPLE-3"))
	ff.setPayload("EXAMPLE-3", minimalIssueJSON("EXAMPLE-3"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 2, sum.Fetched, "Fetched")
	assert.Equal(t, 1, sum.CapLimited, "CapLimited")
	assert.Contains(t, sum.CapLimitedKeys, "EXAMPLE-3", "CapLimitedKeys must contain EXAMPLE-3")
}

// TestCrawl_PermissionDenied (PRD AC 9): A links to B; B returns 403.
// A fetched, B stubbed; B/index.md contains "Permission denied (403)".
func TestCrawl_PermissionDenied(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setError("EXAMPLE-2", client.ErrForbidden)
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Fetched, "Fetched")
	assert.Equal(t, 1, sum.Stubbed, "Stubbed")

	stubPath := filepath.Join(cfg.OutputDir, "EXAMPLE-2", "index.md")
	content, err := os.ReadFile(stubPath)
	require.NoError(t, err, "EXAMPLE-2/index.md must be readable")
	assert.Contains(t, string(content), "Permission denied (403)", "stub content")

	assert.Equal(t, 1, countEvents(sink, events.KindIssueStubbed), "KindIssueStubbed count")
}

// TestCrawl_NotFoundStub: same as PermissionDenied but with 404.
func TestCrawl_NotFoundStub(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setError("EXAMPLE-2", client.ErrNotFound)
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Stubbed, "Stubbed")

	stubPath := filepath.Join(cfg.OutputDir, "EXAMPLE-2", "index.md")
	content, err := os.ReadFile(stubPath)
	require.NoError(t, err, "EXAMPLE-2/index.md must be readable")
	assert.Contains(t, string(content), "Not found (404)", "stub content")
}

// TestCrawl_SkipIfExists (PRD AC 10): pre-create index.md; refetch=false.
// Fetcher must NOT be called; Skipped == 1; KindIssueSkipped emitted.
func TestCrawl_SkipIfExists(t *testing.T) {
	cfg := testConfig(t)
	cfg.Refetch = false

	// Pre-create the index.md.
	issueDir := filepath.Join(cfg.OutputDir, "EXAMPLE-1")
	require.NoError(t, os.MkdirAll(issueDir, 0755))
	originalContent := "# EXAMPLE-1 — original\n"
	require.NoError(t, os.WriteFile(filepath.Join(issueDir, "index.md"), []byte(originalContent), 0644))

	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 0, ff.count("EXAMPLE-1"), "fetcher must not be called")
	assert.Equal(t, 1, sum.Skipped, "Skipped")
	assert.Equal(t, 1, countEvents(sink, events.KindIssueSkipped), "KindIssueSkipped count")

	// File must be untouched.
	content, _ := os.ReadFile(filepath.Join(issueDir, "index.md"))
	assert.Equal(t, originalContent, string(content), "file must not be overwritten")
}

// TestCrawl_RefetchOverride (PRD AC 11): pre-create index.md; refetch=true.
// Fetcher MUST be called; file overwritten with new content.
func TestCrawl_RefetchOverride(t *testing.T) {
	cfg := testConfig(t)
	cfg.Refetch = true

	issueDir := filepath.Join(cfg.OutputDir, "EXAMPLE-1")
	require.NoError(t, os.MkdirAll(issueDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(issueDir, "index.md"), []byte("# old content\n"), 0644))

	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, ff.count("EXAMPLE-1"), "fetcher call count")
	assert.Equal(t, 1, sum.Fetched, "Fetched")

	content, _ := os.ReadFile(filepath.Join(issueDir, "index.md"))
	assert.NotContains(t, string(content), "old content", "file must be overwritten")
}

// TestCrawl_Unauthorized (PRD AC 16): fetcher returns ErrUnauthorized.
// Crawl returns an error wrapping ErrUnauthorized; partial Summary returned.
func TestCrawl_Unauthorized(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setError("EXAMPLE-1", client.ErrUnauthorized)
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.Error(t, err, "expected error")
	assert.ErrorIs(t, err, client.ErrUnauthorized)
	// Partial summary is still returned.
	_ = sum
}

// TestCrawl_FullSuccess (PRD AC 17): two-issue chain, both succeed.
// Crawl returns nil error.
func TestCrawl_FullSuccess(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setPayload("EXAMPLE-2", minimalIssueJSON("EXAMPLE-2"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	_, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	assert.NoError(t, err, "expected nil error")
}

// TestCrawl_GracefulShutdown: slow fetcher (200ms each), cancel after 50ms.
// No panic; no goroutine leak; remaining items appear as CapLimited.
func TestCrawl_GracefulShutdown(t *testing.T) {
	cfg := testConfig(t)
	cfg.Concurrency = 2

	// Build a chain of 5 issues so there is always work in the queue.
	ff := newFakeFetcher()
	ff.delay = 200 * time.Millisecond
	for i := 1; i <= 5; i++ {
		key := fmt.Sprintf("EXAMPLE-%d", i)
		if i < 5 {
			next := fmt.Sprintf("EXAMPLE-%d", i+1)
			ff.setPayload(key, issueWithLinkJSON(key, next))
		} else {
			ff.setPayload(key, minimalIssueJSON(key))
		}
	}
	sink := &events.RecordingSink{}

	goroutinesBefore := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 50ms — before any fetch can complete.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	// err may be nil or context.Canceled — both are acceptable.
	_ = err

	// Give goroutines a moment to exit.
	time.Sleep(300 * time.Millisecond)

	goroutinesAfter := runtime.NumGoroutine()
	// Allow a small tolerance for test framework goroutines.
	assert.LessOrEqual(t, goroutinesAfter, goroutinesBefore+5,
		"possible goroutine leak: before=%d after=%d", goroutinesBefore, goroutinesAfter)
}

// TestCrawl_Concurrency: 5 workers, 20 issues, 50ms each.
// Wall-clock must be well under 20*50ms = 1000ms.
// Exactly 20 distinct fetches; no race detector failures.
func TestCrawl_Concurrency(t *testing.T) {
	cfg := testConfig(t)
	cfg.Concurrency = 5

	ff := newFakeFetcher()
	ff.delay = 50 * time.Millisecond

	// Build a flat list: EXAMPLE-1 links to all others.
	// We use a star topology so all 20 are discovered from the first fetch.
	var links []string
	for i := 2; i <= 20; i++ {
		links = append(links, fmt.Sprintf("EXAMPLE-%d", i))
	}
	ff.setPayload("EXAMPLE-1", issueWithMultipleLinksJSON("EXAMPLE-1", links))
	for i := 2; i <= 20; i++ {
		key := fmt.Sprintf("EXAMPLE-%d", i)
		ff.setPayload(key, minimalIssueJSON(key))
	}
	sink := &events.RecordingSink{}

	start := time.Now()
	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Equal(t, 20, sum.Fetched, "Fetched")

	// With 5 workers and 50ms each, 20 issues should take ~200ms (4 rounds),
	// well under the sequential 1000ms. Allow generous 800ms for CI.
	assert.LessOrEqual(t, elapsed, 800*time.Millisecond,
		"crawl took %s, expected < 800ms (concurrency not working)", elapsed)

	// Verify exactly 20 distinct fetches.
	total := 0
	ff.mu.Lock()
	for _, c := range ff.counts {
		total += c
	}
	ff.mu.Unlock()
	assert.Equal(t, 20, total, "total fetch calls")
}

// TestCrawl_PRDeduplication: two issues both link to the same PR URL.
// PRsFound must be 1, not 2.
func TestCrawl_PRDeduplication(t *testing.T) {
	cfg := testConfig(t)
	prURL := "https://github.com/org/repo/pull/42"

	ff := newFakeFetcher()
	// EXAMPLE-1 links to EXAMPLE-2 and has a PR remote link.
	ff.setPayload("EXAMPLE-1", issueWithLinkAndPRJSON("EXAMPLE-1", "EXAMPLE-2", prURL))
	// EXAMPLE-2 also has the same PR remote link.
	ff.setPayload("EXAMPLE-2", issueWithPRJSON("EXAMPLE-2", prURL))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.PRsFound, "PRsFound")
}

// ---------------------------------------------------------------------------
// Additional fixture helpers used by tests above
// ---------------------------------------------------------------------------

// issueWithMultipleLinksJSON returns a Jira issue JSON for key that has
// outward issue links to all keys in linkedKeys.
func issueWithMultipleLinksJSON(key string, linkedKeys []string) []byte {
	var linkObjs []string
	for _, lk := range linkedKeys {
		linkObjs = append(linkObjs, fmt.Sprintf(
			`{"type":{"name":"Relates","inward":"relates to","outward":"relates to"},"outwardIssue":{"key":%q,"fields":{"summary":"Summary of linked"}}}`,
			lk,
		))
	}
	linksJSON := "[" + strings.Join(linkObjs, ",") + "]"
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": %s,
    "remotelinks": []
  }
}`, key, key, linksJSON))
}

// issueWithLinkAndPRJSON returns a Jira issue JSON for key that has an
// outward issue link to linkedKey and a remote link to prURL.
func issueWithLinkAndPRJSON(key, linkedKey, prURL string) []byte {
	return []byte(fmt.Sprintf(`{
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [
      {
        "type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
        "outwardIssue": {"key": %q, "fields": {"summary": "Summary of linked"}}
      }
    ],
    "remotelinks": [
      {"object": {"title": "PR", "url": %q}}
    ]
  }
}`, key, key, linkedKey, prURL))
}

// ---------------------------------------------------------------------------
// TestCrawl_SummaryKeys: verify FetchedKeys is sorted alphabetically.
// ---------------------------------------------------------------------------

func TestCrawl_SummaryKeys(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	// Seed with keys in reverse order to verify sorting.
	keys := []string{"EXAMPLE-3", "EXAMPLE-1", "EXAMPLE-2"}
	for _, k := range keys {
		ff.setPayload(k, minimalIssueJSON(k))
	}
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, keys, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 3, sum.Fetched, "Fetched")
	assert.True(t, sort.StringsAreSorted(sum.FetchedKeys), "FetchedKeys not sorted: %v", sum.FetchedKeys)
}

// ---------------------------------------------------------------------------
// TestCrawl_RateLimited: fetcher returns ErrRateLimited.
// Issue counted as Failed; crawl continues.
// ---------------------------------------------------------------------------

func TestCrawl_RateLimited(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-2"))
	ff.setError("EXAMPLE-2", client.ErrRateLimited)
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, sum.Fetched, "Fetched")
	assert.Equal(t, 1, sum.Failed, "Failed")
	assert.Contains(t, sum.FailedKeys, "EXAMPLE-2", "FailedKeys must contain EXAMPLE-2")
}

// ---------------------------------------------------------------------------
// TestCrawl_CrawlSummaryEvent: KindCrawlSummary emitted exactly once.
// ---------------------------------------------------------------------------

func TestCrawl_CrawlSummaryEvent(t *testing.T) {
	cfg := testConfig(t)
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	_, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 1, countEvents(sink, events.KindCrawlSummary), "KindCrawlSummary count")

	// The summary event must also carry the structured CrawlSummary so
	// downstream sinks (e.g. the gRPC stream adapter) can deliver typed
	// totals without re-parsing the Message string.
	var summaryEvent events.Event
	for _, e := range sink.Events() {
		if e.Kind == events.KindCrawlSummary {
			summaryEvent = e
			break
		}
	}
	require.NotNil(t, summaryEvent.Summary, "KindCrawlSummary event must carry a structured Summary")
	assert.Equal(t, 1, summaryEvent.Summary.Fetched, "Summary.Fetched should mirror the run")
	assert.Equal(t, []string{"EXAMPLE-1"}, summaryEvent.Summary.FetchedKeys, "Summary.FetchedKeys")
}

// ---------------------------------------------------------------------------
// TestCrawl_NoConcurrentDoubleFetch: with concurrency > 1, the same key
// must never be fetched more than once even if it appears in multiple
// issues' link lists simultaneously.
// ---------------------------------------------------------------------------

func TestCrawl_NoConcurrentDoubleFetch(t *testing.T) {
	cfg := testConfig(t)
	cfg.Concurrency = 4

	// EXAMPLE-1 and EXAMPLE-2 both link to EXAMPLE-3.
	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", issueWithLinkJSON("EXAMPLE-1", "EXAMPLE-3"))
	ff.setPayload("EXAMPLE-2", issueWithLinkJSON("EXAMPLE-2", "EXAMPLE-3"))
	ff.setPayload("EXAMPLE-3", minimalIssueJSON("EXAMPLE-3"))
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1", "EXAMPLE-2"}, ff, sink)
	require.NoError(t, err)
	assert.Equal(t, 3, sum.Fetched, "Fetched")
	assert.Equal(t, 1, ff.count("EXAMPLE-3"), "EXAMPLE-3 fetch count")
}

// ---------------------------------------------------------------------------
// TestCrawl_TimeCap: time cap via cfg.TimeCapSeconds.
// ---------------------------------------------------------------------------

func TestCrawl_TimeCap(t *testing.T) {
	cfg := testConfig(t)
	cfg.TimeCapSeconds = 1 // 1 second cap
	cfg.Concurrency = 1

	// Slow fetcher: each fetch takes 300ms. With 1s cap and 1 worker,
	// we expect at most 3 fetches before the cap fires.
	ff := newFakeFetcher()
	ff.delay = 300 * time.Millisecond
	for i := 1; i <= 10; i++ {
		key := fmt.Sprintf("EXAMPLE-%d", i)
		if i < 10 {
			next := fmt.Sprintf("EXAMPLE-%d", i+1)
			ff.setPayload(key, issueWithLinkJSON(key, next))
		} else {
			ff.setPayload(key, minimalIssueJSON(key))
		}
	}
	sink := &events.RecordingSink{}

	ctx := context.Background()
	sum, err := Crawl(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink)
	// err may be nil or context.DeadlineExceeded — both acceptable.
	_ = err

	// Should have fetched fewer than all 10.
	assert.Less(t, sum.Fetched, 10, "Fetched should be < 10 (time cap should have fired)")
}

// ---------------------------------------------------------------------------
// Compile-time check: fakeFetcher satisfies fetch.Fetcher.
// We import fetch only for this assertion; the test file is in package crawl
// so it can access unexported helpers.
// ---------------------------------------------------------------------------

// Ensure fakeFetcher satisfies the fetch.Fetcher interface at compile time.
// We use a blank import of the fetch package via the interface check below.
var _ interface {
	Fetch(ctx context.Context, key string) ([]byte, error)
} = (*fakeFetcher)(nil)

// Ensure atomic is used (imported for potential future use in concurrency tests).
var _ = atomic.AddInt64

// ---------------------------------------------------------------------------
// fakeDevStatusEnricher — a minimal DevStatusEnricher for the
// partial-failure regression test below.
// ---------------------------------------------------------------------------

// fakeDevStatusEnricher returns a fixed DevStatusData value and a
// fixed (possibly nil) error on every call to Enrich. It is the
// crawl-package-level equivalent of devstatus.Enricher's per-call
// fake; we keep it tiny because only one test in this package needs
// to exercise the enrichment-error path.
type fakeDevStatusEnricher struct {
	data parse.DevStatusData
	err  error
}

func (f *fakeDevStatusEnricher) Enrich(_ context.Context, _ parse.Issue) (parse.DevStatusData, error) {
	return f.data, f.err
}

// numericIssueJSON returns a minimal valid Jira issue JSON for key
// that also carries a top-level "id" field. The id is required to
// trigger Dev Status enrichment in the crawl orchestrator: an empty
// NumericID short-circuits the enricher entirely.
func numericIssueJSON(key, numericID string) []byte {
	return []byte(fmt.Sprintf(`{
  "id": %q,
  "key": %q,
  "self": "https://example.atlassian.net/rest/api/3/issue/%s",
  "fields": {
    "summary": "Summary of %s",
    "status": {"name": "Open"},
    "issuetype": {"name": "Task"},
    "assignee": null,
    "reporter": {"displayName": "Alice Example", "emailAddress": "alice@example.com"},
    "created": "2026-01-01T00:00:00.000+0000",
    "updated": "2026-01-01T00:00:00.000+0000",
    "description": null,
    "parent": null,
    "subtasks": [],
    "issuelinks": [],
    "remotelinks": []
  }
}`, numericID, key, numericID, key))
}

// TestCrawl_DevStatusPartialFailure is the PLATENG-1417 regression
// test at the crawl-orchestrator layer. The DevStatusEnricher returns
// partial data (one pull request) alongside a non-fatal joined error
// (one of the per-dataType calls failed to unmarshal). The crawl
// orchestrator must:
//
//   - emit exactly one KindDevStatusPartialFailure event for the issue
//     (not KindIssueFailed) so log filters / alerting do not conflate
//     this with an issue-level failure;
//   - count the issue as Fetched, not Failed (Summary.Failed == 0,
//     Summary.Fetched == 1);
//   - render the issue normally with the partial Dev Status data
//     attached to issue.DevStatus (so the rendered Markdown still
//     surfaces the entities that did come back).
func TestCrawl_DevStatusPartialFailure(t *testing.T) {
	cfg := testConfig(t)
	cfg.IncludeDevStatus = true
	cfg.DevStatusApplications = []string{"GitHub"}
	cfg.DevStatusDataTypes = []string{"pullrequest", "branch", "commit", "repository", "build"}

	ff := newFakeFetcher()
	ff.setPayload("EXAMPLE-1", numericIssueJSON("EXAMPLE-1", "86679"))

	// Enricher returns 1 PR + a non-fatal joined error (mirrors the
	// devstatus.Enricher contract: at least one entity collected →
	// data + non-nil error).
	enricher := &fakeDevStatusEnricher{
		data: parse.DevStatusData{
			PullRequests: []parse.PullRequest{{
				ID:          "#1",
				URL:         "https://github.com/org/repo/pull/1",
				Title:       "fix things",
				Status:      "MERGED",
				Application: "GitHub",
				Repository:  "org/repo",
			}},
		},
		err: errors.New("devstatus: GitHub/commit for EXAMPLE-1: client: unmarshal dev status response"),
	}

	sink := &events.RecordingSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sum, err := CrawlWithEnrichers(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink, nil, enricher, nil, nil)
	require.NoError(t, err, "Crawl must not return a fatal error for a partial enrichment failure")

	// Summary semantics: the issue was rendered, NOT failed.
	assert.Equal(t, 1, sum.Fetched, "Summary.Fetched")
	assert.Equal(t, 0, sum.Failed, "Summary.Failed must NOT include a partial enrichment failure")
	assert.NotContains(t, sum.FailedKeys, "EXAMPLE-1", "FailedKeys must not include the issue")

	// Event taxonomy: exactly one KindDevStatusPartialFailure, zero
	// KindIssueFailed for this key.
	assert.Equal(t, 1, countEvents(sink, events.KindDevStatusPartialFailure),
		"exactly one KindDevStatusPartialFailure expected")
	for _, e := range sink.Events() {
		if e.Kind == events.KindIssueFailed {
			t.Errorf("no KindIssueFailed event expected, got one with message %q", e.Message)
		}
	}

	// And PR-found accounting still works: the partial data flowed
	// through to the post-render Dev-Status PR accounting block.
	assert.Equal(t, 1, sum.PRsFound, "the partial PR result was still counted")

	// Disk side-effect: index.md exists for the issue (proves render
	// + write completed despite the enrichment error).
	indexPath := filepath.Join(cfg.OutputDir, "EXAMPLE-1", "index.md")
	_, statErr := os.Stat(indexPath)
	require.NoError(t, statErr, "index.md must be written")
	content, readErr := os.ReadFile(indexPath)
	require.NoError(t, readErr)
	md := string(content)
	assert.Contains(t, md, "## Development", "Development section rendered from partial data")
	assert.Contains(t, md, "https://github.com/org/repo/pull/1",
		"the PR that did come back is rendered")
}

// ---------------------------------------------------------------------------
// TestCrawl_InjectedStore — Phase 1 seam-4 coverage.
// ---------------------------------------------------------------------------

// recordingStore is a minimal output.Store implementation that records
// the keys it was asked to write. It deliberately does NOT touch the
// filesystem, so a successful run proves that the injected store, and
// only the injected store, received the crawl output.
type recordingStore struct {
	mu      sync.Mutex
	written []string
}

func (r *recordingStore) Write(_ context.Context, key, _, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.written = append(r.written, key)
	return nil
}

func (r *recordingStore) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.written))
	copy(out, r.written)
	sort.Strings(out)
	return out
}

// TestCrawl_InjectedStore exercises the seam-4 additive widening of
// CrawlWithEnrichers:
//
//   - When a non-nil store is supplied, every successful issue write
//     goes through it (and nothing is written to cfg.OutputDir).
//   - When nil is supplied, the historical FSStore default kicks in
//     and the index.md lands under cfg.OutputDir.
func TestCrawl_InjectedStore(t *testing.T) {
	t.Run("injected store receives writes and skips disk", func(t *testing.T) {
		cfg := testConfig(t)
		ff := newFakeFetcher()
		ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
		sink := &events.RecordingSink{}
		store := &recordingStore{}

		ctx := context.Background()
		sum, err := CrawlWithEnrichers(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink, nil, nil, store, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, sum.Fetched, "Fetched")
		assert.Equal(t, 0, sum.Failed, "Failed")

		// The injected store saw the write.
		assert.Equal(t, []string{"EXAMPLE-1"}, store.keys(), "injected store must record the issue key")

		// And nothing landed on disk — the fake store does not write,
		// so cfg.OutputDir must still be empty for this run.
		indexPath := filepath.Join(cfg.OutputDir, "EXAMPLE-1", "index.md")
		_, statErr := os.Stat(indexPath)
		assert.True(t, errors.Is(statErr, os.ErrNotExist),
			"no file must be written to cfg.OutputDir when an injected store is supplied; got err=%v", statErr)
	})

	t.Run("nil store defaults to FSStore writing to disk", func(t *testing.T) {
		cfg := testConfig(t)
		ff := newFakeFetcher()
		ff.setPayload("EXAMPLE-1", minimalIssueJSON("EXAMPLE-1"))
		sink := &events.RecordingSink{}

		ctx := context.Background()
		sum, err := CrawlWithEnrichers(ctx, cfg, []string{"EXAMPLE-1"}, ff, sink, nil, nil, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 1, sum.Fetched, "Fetched")

		// The historical on-disk behavior must be preserved.
		indexPath := filepath.Join(cfg.OutputDir, "EXAMPLE-1", "index.md")
		_, statErr := os.Stat(indexPath)
		assert.NoError(t, statErr, "index.md must exist at %s when nil store defaults to FSStore", indexPath)
	})

	// Silence the unused-import linter in the rare case a future
	// refactor drops the only other reference to output. The Store
	// type assertion ensures recordingStore actually satisfies the
	// interface that CrawlWithEnrichers expects.
	var _ output.Store = (*recordingStore)(nil)
}
