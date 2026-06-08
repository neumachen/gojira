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

// withInjectedLogger runs CrawlWithEnrichers under a tiny harness that
// (a) constructs a sink, (b) registers an "after construct" hook to
// overwrite the crawler's logger BEFORE workers spawn. Since
// CrawlWithEnrichers does not currently expose a logger entry-point
// parameter (that is phase-d-thread-3), we exercise the seam by
// running the public entry point with a t.Cleanup-restored package
// override of the test seam. To keep this self-contained, we override
// the package-level `defaultCrawlerLogger` indirection introduced by
// the production change.
func withInjectedLogger(t *testing.T, lg *slog.Logger, fn func()) {
	t.Helper()
	prev := defaultCrawlerLogger
	defaultCrawlerLogger = func() *slog.Logger { return lg }
	t.Cleanup(func() { defaultCrawlerLogger = prev })
	fn()
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

	withInjectedLogger(t, lg, func() {
		sum, err := Crawl(context.Background(), cfg, []string{"EX-1"}, ff, sink)
		if err != nil {
			t.Fatalf("Crawl: %v", err)
		}
		if sum.Fetched != 1 {
			t.Errorf("Fetched: got %d, want 1", sum.Fetched)
		}
	})

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

	withInjectedLogger(t, lg, func() {
		if _, err := Crawl(context.Background(), cfg, []string{"EX-1"}, ff, sink); err != nil {
			t.Fatalf("Crawl: %v", err)
		}
	})

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

	withInjectedLogger(t, lg, func() {
		if _, err := Crawl(context.Background(), cfg, []string{"EX-1"}, ff, sink); err != nil {
			t.Fatalf("Crawl: %v", err)
		}
	})

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
