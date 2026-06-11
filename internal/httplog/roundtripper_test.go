package httplog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/neumachen/gojira/internal/httplog"
	"github.com/neumachen/gojira/internal/trace"
	gojiralog "github.com/neumachen/gojira/pkg/log"
)

// ---------------------------------------------------------------------------
// Plumbing helpers
// ---------------------------------------------------------------------------

// captureLogger returns a JSON logger writing into buf at level lv.
func captureLogger(buf *bytes.Buffer, lv slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: lv}))
}

// recordsOf decodes every JSON record from buf into a slice of maps.
// Lines that fail to decode are skipped (defensive against trailing
// newlines or partial writes).
func recordsOf(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// Stop on first decode error; the test will assert based
			// on what we've successfully decoded so far.
			break
		}
		out = append(out, rec)
	}
	return out
}

// findFirst returns the first record whose msg field matches.
func findFirst(records []map[string]any, msg string) map[string]any {
	for _, r := range records {
		if got, _ := r["msg"].(string); got == msg {
			return r
		}
	}
	return nil
}

// roundTripWith builds a *httplog.RoundTripper around srv's transport and
// performs a single GET against srv.URL+path with optional headers. It
// returns the response body bytes (after the round-tripper re-wrapped the
// body) so callers can assert the body is still readable.
func roundTripWith(t *testing.T, rt *httplog.RoundTripper, srv *httptest.Server, path string, headers http.Header) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return body
}

// ---------------------------------------------------------------------------
// INFO summary
// ---------------------------------------------------------------------------

func TestRoundTripper_InfoSummary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, slog.LevelInfo))

	body := roundTripWith(t, rt, srv, "/x", nil)
	if string(body) != "ok" {
		t.Errorf("body: got %q, want %q", string(body), "ok")
	}

	recs := recordsOf(t, &buf)
	resp := findFirst(recs, "http.response")
	if resp == nil {
		t.Fatalf("expected http.response record; got: %v", recs)
	}

	// Required attrs on the summary line.
	if got, _ := resp["http_method"].(string); got != http.MethodGet {
		t.Errorf("http_method: got %v", resp["http_method"])
	}
	if got, _ := resp["url"].(string); got != "/x" {
		t.Errorf("url: got %v, want /x", resp["url"])
	}
	if got, _ := resp["status"].(float64); int(got) != http.StatusOK {
		t.Errorf("status: got %v", resp["status"])
	}
	if _, ok := resp["duration_ms"]; !ok {
		t.Errorf("duration_ms missing: %v", resp)
	}
	if got, _ := resp["bytes"].(float64); int(got) != 2 {
		t.Errorf("bytes: got %v, want 2", resp["bytes"])
	}

	// At INFO, the TRACE start/complete lines must NOT have been emitted.
	if findFirst(recs, "http.request.start") != nil {
		t.Errorf("http.request.start must be suppressed at INFO level")
	}
	if findFirst(recs, "http.request.complete") != nil {
		t.Errorf("http.request.complete must be suppressed at INFO level")
	}
}

// ---------------------------------------------------------------------------
// TRACE full lifecycle
// ---------------------------------------------------------------------------

func TestRoundTripper_TraceFullLifecycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, gojiralog.LevelTrace))

	roundTripWith(t, rt, srv, "/y", nil)

	recs := recordsOf(t, &buf)
	if findFirst(recs, "http.request.start") == nil {
		t.Errorf("http.request.start missing at LevelTrace; recs=%v", recs)
	}
	if findFirst(recs, "http.response") == nil {
		t.Errorf("http.response missing at LevelTrace")
	}
	complete := findFirst(recs, "http.request.complete")
	if complete == nil {
		t.Fatalf("http.request.complete missing at LevelTrace")
	}
	if got, _ := complete["response_body"].(string); got != "ok" {
		t.Errorf("response_body: got %q, want \"ok\"", got)
	}
	// The httptrace timing keys must be present (value may be -1 if a
	// phase did not run, e.g. conn reuse skips DNS).
	for _, k := range []string{"dns_ms", "connect_ms", "ttfb_ms"} {
		if _, ok := complete[k]; !ok {
			t.Errorf("complete record missing %q", k)
		}
	}
}

// ---------------------------------------------------------------------------
// Redaction audit — Authorization and Cookie are NEVER leaked
// ---------------------------------------------------------------------------

func TestRoundTripper_RedactsAuthorizationEvenAtTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, gojiralog.LevelTrace))

	const token = "c2VjcmV0OnRva2Vu" // base64("secret:token")
	headers := http.Header{
		"Authorization": []string{"Basic " + token},
		"Cookie":        []string{"session=topsecret"},
	}
	roundTripWith(t, rt, srv, "/z", headers)

	captured := buf.String()
	// The token bytes themselves MUST NOT appear anywhere — not in
	// headers, not in URL, not in any incidental line.
	if strings.Contains(captured, token) {
		t.Errorf("captured output leaks the Basic-auth token (%q); buf:\n%s", token, captured)
	}
	if strings.Contains(captured, "Basic c2VjcmV0") {
		t.Errorf("captured output leaks the Authorization value prefix; buf:\n%s", captured)
	}
	if strings.Contains(captured, "topsecret") {
		t.Errorf("captured output leaks the Cookie value; buf:\n%s", captured)
	}
	if !strings.Contains(captured, "REDACTED") {
		t.Errorf("expected REDACTED placeholder somewhere in trace output; buf:\n%s", captured)
	}
}

// ---------------------------------------------------------------------------
// Body re-wrap — downstream consumers still get the original body
// ---------------------------------------------------------------------------

func TestRoundTripper_BodyIsStillReadableAfterLogging(t *testing.T) {
	const payload = "the quick brown fox"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, gojiralog.LevelTrace))

	got := string(roundTripWith(t, rt, srv, "/body", nil))
	if got != payload {
		t.Errorf("body round-trip: got %q, want %q", got, payload)
	}
}

// ---------------------------------------------------------------------------
// Logger source — ctx > configured
// ---------------------------------------------------------------------------

func TestRoundTripper_NoCtxLoggerFallsBackToConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, slog.LevelInfo))

	roundTripWith(t, rt, srv, "/x", nil)

	if buf.Len() == 0 {
		t.Fatal("expected the configured logger to receive the log line")
	}
	if findFirst(recordsOf(t, &buf), "http.response") == nil {
		t.Errorf("configured logger did not receive http.response; buf:\n%s", buf.String())
	}
}

func TestRoundTripper_CtxLoggerWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var ctxBuf, cfgBuf bytes.Buffer
	ctxLogger := captureLogger(&ctxBuf, slog.LevelInfo)
	cfgLogger := captureLogger(&cfgBuf, slog.LevelInfo)
	rt := httplog.New(srv.Client().Transport, cfgLogger)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/wins", nil)
	req = req.WithContext(trace.WithLogger(context.Background(), ctxLogger))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if findFirst(recordsOf(t, &ctxBuf), "http.response") == nil {
		t.Errorf("ctx logger must receive the line; ctxBuf:\n%s", ctxBuf.String())
	}
	if cfgBuf.Len() != 0 {
		t.Errorf("configured logger must be skipped when ctx supplies one; cfgBuf:\n%s", cfgBuf.String())
	}
}

// ---------------------------------------------------------------------------
// trace_stream attribute is on the wire
// ---------------------------------------------------------------------------

func TestRoundTripper_TraceStreamAttrPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(srv.Client().Transport, captureLogger(&buf, slog.LevelInfo))
	roundTripWith(t, rt, srv, "/ts", nil)

	resp := findFirst(recordsOf(t, &buf), "http.response")
	if resp == nil {
		t.Fatal("missing http.response record")
	}
	if got, _ := resp[trace.AttrTraceStream].(string); got != trace.StreamResponse {
		t.Errorf("%s: got %v, want %q", trace.AttrTraceStream, resp[trace.AttrTraceStream], trace.StreamResponse)
	}
}

// ---------------------------------------------------------------------------
// Transport error path
// ---------------------------------------------------------------------------

// errRoundTripper is a base http.RoundTripper that always fails. Used to
// exercise the failure-logging branch without involving the network.
type errRoundTripper struct{ err error }

func (e *errRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, e.err
}

func TestRoundTripper_TransportError(t *testing.T) {
	var buf bytes.Buffer
	want := errors.New("transport boom")
	rt := httplog.New(&errRoundTripper{err: want}, captureLogger(&buf, slog.LevelInfo))

	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/fail", nil)
	_, err := rt.RoundTrip(req)
	if !errors.Is(err, want) {
		t.Errorf("RoundTrip error: got %v, want %v", err, want)
	}

	rec := findFirst(recordsOf(t, &buf), "http.request.failed")
	if rec == nil {
		t.Fatalf("expected http.request.failed record; buf:\n%s", buf.String())
	}
	if got, _ := rec["error"].(string); got != want.Error() {
		t.Errorf("error attr: got %q, want %q", got, want.Error())
	}
}

// ---------------------------------------------------------------------------
// nil Base falls back to http.DefaultTransport
// ---------------------------------------------------------------------------

func TestRoundTripper_NilBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	var buf bytes.Buffer
	rt := httplog.New(nil, captureLogger(&buf, slog.LevelInfo))

	body := roundTripWith(t, rt, srv, "/nil-base", nil)
	if string(body) != "ok" {
		t.Errorf("body: got %q, want %q", string(body), "ok")
	}
	if findFirst(recordsOf(t, &buf), "http.response") == nil {
		t.Error("expected http.response record with nil Base")
	}
}
