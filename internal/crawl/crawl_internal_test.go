// White-box span-instrumentation tests for the crawl orchestrator.
// They live in package crawl so they can construct a *crawler directly
// and assign the unexported `logger` and `rootSpan` fields before
// running CrawlWithEnrichers. The black-box tests in crawl_test.go
// (also package crawl in this repo, but exercising public entry
// points only) guard byte-identical behavior; this file asserts the
// new observability output.
package crawl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/neumachen/gojira/classify"
	"github.com/neumachen/gojira/internal/config"
	"github.com/neumachen/gojira/internal/events"
	gojiralog "github.com/neumachen/gojira/log"
)

// ---------------------------------------------------------------------------
// Local fake fetcher (separate from crawl_test.go's to keep the tests
// independent and avoid coupling).
// ---------------------------------------------------------------------------

type spanFakeFetcher struct {
	payloads map[string][]byte
}

func (f *spanFakeFetcher) Fetch(_ context.Context, key string) ([]byte, error) {
	if b, ok := f.payloads[key]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("spanFakeFetcher: no payload for %q", key)
}

// spanIssueJSON returns a minimal valid issue body with an optional
// outward "Relates" link to linkedKey (empty linkedKey ⇒ no link).
func spanIssueJSON(key, linkedKey string) []byte {
	link := ""
	if linkedKey != "" {
		link = fmt.Sprintf(`{
			"type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
			"outwardIssue": {"key": %q, "fields": {"summary": "linked"}}
		}`, linkedKey)
	}
	return []byte(fmt.Sprintf(`{
		"key": %q,
		"self": "https://example.atlassian.net/rest/api/3/issue/10001",
		"fields": {
			"summary": "Summary of %s",
			"status": {"name": "Open"},
			"issuetype": {"name": "Task"},
			"assignee": null,
			"reporter": {"displayName": "Alice", "emailAddress": "a@example.com"},
			"created": "2026-01-01T00:00:00.000+0000",
			"updated": "2026-01-01T00:00:00.000+0000",
			"description": null,
			"parent": null,
			"subtasks": [],
			"issuelinks": [%s],
			"remotelinks": []
		}
	}`, key, key, link))
}

// captureLogger returns a JSON logger writing into buf at level lv.
func captureLogger(buf *bytes.Buffer, lv slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: lv}))
}

// recordsOf decodes every JSON record from buf into a slice of maps.
func recordsOf(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec map[string]any
		err := dec.Decode(&rec)
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		out = append(out, rec)
	}
	return out
}

func findAllByMsg(records []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range records {
		if got, _ := r["msg"].(string); got == msg {
			out = append(out, r)
		}
	}
	return out
}

// crawlWithLogger runs CrawlWithEnrichers with a captured *slog.Logger
// installed via the new explicit logger parameter (phase-d-thread-3).
// All hierarchy / dev-status / store dependencies are passed nil so the
// orchestrator only exercises the always-on phases (fetch/parse/render
// /store). This replaces the former package-level `defaultCrawlerLogger`
// seam, which was a stop-gap and has been removed.
func crawlWithLogger(
	ctx context.Context,
	cfg config.Config,
	keys []string,
	f *spanFakeFetcher,
	sink events.Sink,
	lg *slog.Logger,
) (Summary, error) {
	return CrawlWithEnrichers(ctx, cfg, keys, f, sink, nil, nil, nil, lg)
}

// ---------------------------------------------------------------------------
// crawl.start / crawl.end + per-issue spans + per-phase blocks
// ---------------------------------------------------------------------------

func TestSpanInstrumentation_SingleIssue_SpanEnvelope(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", ""),
	}}
	sink := &events.RecordingSink{}

	var buf bytes.Buffer
	lg := captureLogger(&buf, slog.LevelInfo)

	sum, err := crawlWithLogger(context.Background(), cfg, []string{"EX-1"}, ff, sink, lg)
	if err != nil {
		t.Fatalf("CrawlWithEnrichers: %v", err)
	}
	if sum.Fetched != 1 {
		t.Errorf("Fetched: got %d, want 1", sum.Fetched)
	}

	recs := recordsOf(t, &buf)
	if len(recs) == 0 {
		t.Fatalf("expected log records; buf:\n%s", buf.String())
	}

	// Every line emitted by the orchestrator must carry trace_stream=stream.
	for _, r := range recs {
		if got, _ := r["trace_stream"].(string); got != "stream" {
			t.Errorf("trace_stream: got %q on record %v", got, r)
		}
	}

	for _, msg := range []string{
		"crawl.start",
		"issue.process.start",
		"phase.start",
		"phase.end",
		"issue.process.end",
		"crawl.end",
	} {
		if findAllByMsg(recs, msg) == nil {
			t.Errorf("expected at least one %q record; buf:\n%s", msg, buf.String())
		}
	}

	// crawl.end must carry duration_ms.
	end := findAllByMsg(recs, "crawl.end")
	if _, ok := end[0]["duration_ms"]; !ok {
		t.Errorf("crawl.end missing duration_ms: %v", end[0])
	}

	// At least one phase.end must report phase=fetch and ok=true.
	var fetchOK bool
	for _, r := range findAllByMsg(recs, "phase.end") {
		if r["phase"] == "fetch" && r["ok"] == true {
			fetchOK = true
			break
		}
	}
	if !fetchOK {
		t.Errorf("expected a phase.end with phase=fetch ok=true; got %v", findAllByMsg(recs, "phase.end"))
	}
}

// ---------------------------------------------------------------------------
// TRACE-level fan-out lineage on enqueue
// ---------------------------------------------------------------------------

func TestSpanInstrumentation_FanoutLineageAtTrace(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", "EX-2"),
		"EX-2": spanIssueJSON("EX-2", ""),
	}}
	sink := &events.RecordingSink{}

	var buf bytes.Buffer
	lg := captureLogger(&buf, gojiralog.LevelTrace)

	if _, err := crawlWithLogger(context.Background(), cfg, []string{"EX-1"}, ff, sink, lg); err != nil {
		t.Fatalf("CrawlWithEnrichers: %v", err)
	}

	fanouts := findAllByMsg(recordsOf(t, &buf), "crawl.fanout")
	if len(fanouts) == 0 {
		t.Fatalf("expected at least one crawl.fanout record; buf:\n%s", buf.String())
	}
	// Locate the EX-2 fan-out and check its discovered_from + depth.
	var hit map[string]any
	for _, r := range fanouts {
		if r["ticket_id"] == "EX-2" {
			hit = r
			break
		}
	}
	if hit == nil {
		t.Fatalf("expected a crawl.fanout for EX-2; got %v", fanouts)
	}
	if got, _ := hit["discovered_from"].(string); !strings.EqualFold(got, "EX-1") {
		t.Errorf("discovered_from: got %q, want EX-1", got)
	}
	if got, _ := hit["depth"].(float64); int(got) != 1 {
		t.Errorf("depth: got %v, want 1", got)
	}
	if got, _ := hit["relation"].(string); got == "" {
		t.Errorf("relation must be non-empty on fan-out; got %v", hit)
	}
}

// ---------------------------------------------------------------------------
// No-op invariant — nothing injected ⇒ behavior unchanged + zero output
// ---------------------------------------------------------------------------

func TestSpanInstrumentation_NoLogger_NoopInvariant(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", ""),
	}}
	sink := &events.RecordingSink{}

	// No withInjectedLogger here: the production default (noop) is in
	// force. The crawl must still succeed and Summary must report
	// exactly the same as the equivalent baseline run.
	sum, err := Crawl(context.Background(), cfg, []string{"EX-1"}, ff, sink)
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if sum.Fetched != 1 {
		t.Errorf("Fetched: got %d, want 1 (instrumentation must not change counts)", sum.Fetched)
	}
	if sum.Failed != 0 {
		t.Errorf("Failed: got %d, want 0", sum.Failed)
	}
	if sum.Stubbed != 0 {
		t.Errorf("Stubbed: got %d, want 0", sum.Stubbed)
	}
	if sum.CapLimited != 0 {
		t.Errorf("CapLimited: got %d, want 0", sum.CapLimited)
	}
}

// ---------------------------------------------------------------------------
// DEBUG skip-if-exists diagnostic
// ---------------------------------------------------------------------------

func TestSpanInstrumentation_SkipIfExists_DebugLine(t *testing.T) {
	outputDir := t.TempDir()
	// Pre-create the issue's index.md so the skip-if-exists branch fires.
	issueDir := outputDir + "/EX-1"
	if err := os.MkdirAll(issueDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(issueDir+"/index.md", []byte("already here"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   outputDir,
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{}}
	sink := &events.RecordingSink{}

	var buf bytes.Buffer
	lg := captureLogger(&buf, slog.LevelDebug)

	if _, err := crawlWithLogger(context.Background(), cfg, []string{"EX-1"}, ff, sink, lg); err != nil {
		t.Fatalf("CrawlWithEnrichers: %v", err)
	}

	hits := findAllByMsg(recordsOf(t, &buf), "crawl.skip_if_exists")
	if len(hits) == 0 {
		t.Fatalf("expected a crawl.skip_if_exists DEBUG record; buf:\n%s", buf.String())
	}
	if got, _ := hits[0]["ticket_id"].(string); got != "EX-1" {
		t.Errorf("ticket_id: got %q, want EX-1", got)
	}

	// Sentinel: classify import is still used by other tests; keep the
	// linter happy if a future refactor drops the other reference.
	_ = classify.KindJiraKey
}

// ---------------------------------------------------------------------------
// phase-d-thread-3: CrawlWithEnrichers logger-parameter widening
// ---------------------------------------------------------------------------

// TestCrawlWithEnrichers_LoggerWidening_NilDefaultsToNoop asserts that the
// new final `logger *slog.Logger` parameter, when passed as nil, defaults
// to a no-op handler so the crawl still runs to completion with the same
// observable Summary counts. This is the additive-widening invariant.
func TestCrawlWithEnrichers_LoggerWidening_NilDefaultsToNoop(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", ""),
	}}
	sink := &events.RecordingSink{}

	// Call CrawlWithEnrichers directly with logger=nil. The orchestrator
	// must (a) not panic, (b) traverse to completion, and (c) report the
	// same Summary counts as the Crawl wrapper would.
	sum, err := CrawlWithEnrichers(
		context.Background(),
		cfg,
		[]string{"EX-1"},
		ff,
		sink,
		nil, // hier
		nil, // devStatus
		nil, // store
		nil, // logger — exercising the nil-default seam
	)
	if err != nil {
		t.Fatalf("CrawlWithEnrichers(logger=nil): %v", err)
	}
	if sum.Fetched != 1 {
		t.Errorf("Fetched: got %d, want 1 (nil logger must not change counts)", sum.Fetched)
	}
	if sum.Failed != 0 {
		t.Errorf("Failed: got %d, want 0", sum.Failed)
	}
}

// ---------------------------------------------------------------------------
// phase-f-measure-1: per-call-type measurement aggregation on Summary
// ---------------------------------------------------------------------------

// TestPhaseMeasurement_Aggregated asserts that after a single-issue crawl,
// the Summary surfaces non-empty per-phase tallies for the always-on
// orchestrator-side phases (fetch, parse, render, store) and that a
// `crawl.measurement` INFO record is emitted carrying the totals.
func TestPhaseMeasurement_Aggregated(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", ""),
	}}
	sink := &events.RecordingSink{}

	var buf bytes.Buffer
	lg := captureLogger(&buf, slog.LevelInfo)

	sum, err := CrawlWithEnrichers(
		context.Background(),
		cfg,
		[]string{"EX-1"},
		ff,
		sink,
		nil, // hier
		nil, // devStatus
		nil, // store
		lg,  // logger — exercise the new explicit injection seam
	)
	if err != nil {
		t.Fatalf("CrawlWithEnrichers: %v", err)
	}
	if sum.Fetched != 1 {
		t.Fatalf("Fetched: got %d, want 1", sum.Fetched)
	}

	// Every always-on phase for a single successful issue must have
	// produced at least one recorded invocation.
	for _, phase := range []string{"fetch", "parse", "render", "store"} {
		count, ok := sum.APICallCounts[phase]
		if !ok {
			t.Errorf("APICallCounts missing key %q; got map=%v", phase, sum.APICallCounts)
			continue
		}
		if count < 1 {
			t.Errorf("APICallCounts[%q] = %d; want >= 1", phase, count)
		}
		if _, ok := sum.APITimeByPhase[phase]; !ok {
			t.Errorf("APITimeByPhase missing key %q; got map=%v", phase, sum.APITimeByPhase)
		}
	}

	// TotalAPITime must equal the sum of APITimeByPhase values.
	var sumTime int64
	for _, d := range sum.APITimeByPhase {
		sumTime += int64(d)
	}
	if int64(sum.TotalAPITime) != sumTime {
		t.Errorf("TotalAPITime=%v != sum(APITimeByPhase)=%v",
			sum.TotalAPITime, sumTime)
	}

	// A crawl.measurement INFO line must be emitted carrying the
	// expected attrs.
	hits := findAllByMsg(recordsOf(t, &buf), "crawl.measurement")
	if len(hits) != 1 {
		t.Fatalf("expected exactly 1 crawl.measurement record; got %d; buf:\n%s",
			len(hits), buf.String())
	}
	rec := hits[0]
	for _, attr := range []string{"total_api_time_ms", "total_duration_ms", "call_counts", "time_by_phase_ms"} {
		if _, ok := rec[attr]; !ok {
			t.Errorf("crawl.measurement missing attr %q; record=%v", attr, rec)
		}
	}
}

// TestPhaseMeasurement_HierarchyOnlyWhenEnabled asserts that the
// hierarchy_jql key is absent from APICallCounts when no hierarchy
// discoverer is wired (i.e. the phase never ran). This guards against
// accidental zero-valued entries that would mislead consumers.
func TestPhaseMeasurement_HierarchyOnlyWhenEnabled(t *testing.T) {
	cfg := config.Config{
		Site:        "https://example.atlassian.net",
		User:        "u@example.com",
		Token:       "t",
		OutputDir:   t.TempDir(),
		Concurrency: 1,
		// IncludeChildren is false by default; even if it were true,
		// hier=nil short-circuits the phase, so the key must stay absent.
	}
	ff := &spanFakeFetcher{payloads: map[string][]byte{
		"EX-1": spanIssueJSON("EX-1", ""),
	}}
	sink := &events.RecordingSink{}

	sum, err := CrawlWithEnrichers(
		context.Background(),
		cfg,
		[]string{"EX-1"},
		ff,
		sink,
		nil, // hier — phase must not record
		nil, // devStatus
		nil, // store
		nil, // logger
	)
	if err != nil {
		t.Fatalf("CrawlWithEnrichers: %v", err)
	}
	if _, present := sum.APICallCounts["hierarchy_jql"]; present {
		t.Errorf("APICallCounts must not contain hierarchy_jql when no discoverer is wired; got %v", sum.APICallCounts)
	}
	if _, present := sum.APITimeByPhase["hierarchy_jql"]; present {
		t.Errorf("APITimeByPhase must not contain hierarchy_jql when no discoverer is wired; got %v", sum.APITimeByPhase)
	}
	if _, present := sum.APICallCounts["dev_status"]; present {
		t.Errorf("APICallCounts must not contain dev_status when no enricher is wired; got %v", sum.APICallCounts)
	}
}
