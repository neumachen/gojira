// Package httplog wraps an http.RoundTripper to emit gojira's structured,
// correlatable, credential-redacted view of every HTTP request the client
// makes. It is the response-stream half of the crawl observability instrument
// (the orchestration-stream half lives in internal/crawl).
//
// Two log levels matter here:
//
//   - INFO: one summary line per request — method, URL path, status,
//     duration_ms, bytes — always emitted. This is the at-a-glance
//     measurement record per call.
//
//   - LevelTrace (from gojira's log package, below slog.LevelDebug):
//     the full lifecycle — net/http/httptrace timings (DNS, connect, TLS,
//     TTFB, conn-reuse) plus the raw response body and response headers.
//     The httptrace machinery is only installed when the active logger is
//     enabled at LevelTrace, keeping the hot path allocation-light on
//     normal info-level runs.
//
// Every record is tagged with trace_stream="response" so it can be filtered
// alongside the orchestration-stream events the crawl emits.
//
// Credential redaction is ABSOLUTE: Authorization, Proxy-Authorization,
// Cookie, Set-Cookie, and X-Atlassian-Token are replaced with the literal
// "REDACTED" wherever they appear, including at trace level. The crawl
// observability PRD enforces this as a non-negotiable invariant; tests in
// this package grep the captured buffer for the raw token bytes and fail
// if they leak.
package httplog

import (
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"github.com/neumachen/gojira/internal/trace"
	"github.com/neumachen/gojira/log"
)

// RoundTripper wraps a base [http.RoundTripper] and logs every request's
// lifecycle through slog with redaction. It is intended to be installed
// on the gojira client via client.WithLogger / client.WithRoundTripper
// for crawl observability. The transport is concurrency-safe iff the
// base transport is.
type RoundTripper struct {
	// Base is the underlying transport. When nil, [http.DefaultTransport]
	// is used at RoundTrip time. Wrapping rather than embedding keeps the
	// composition explicit at call sites.
	Base http.RoundTripper

	// Logger is the fallback sink when the per-request context does NOT
	// carry a logger (see [trace.LoggerFrom]). When both are nil,
	// [slog.Default] is used.
	Logger *slog.Logger
}

// New constructs a RoundTripper. Either argument may be nil; sensible
// defaults are applied at RoundTrip time so the type is total — no
// configuration required to get a working instance.
func New(base http.RoundTripper, logger *slog.Logger) *RoundTripper {
	return &RoundTripper{Base: base, Logger: logger}
}

// RoundTrip executes req through the base transport, logging the request
// summary at INFO and (when the active logger is enabled at LevelTrace)
// the full httptrace lifecycle and raw response body. The returned
// *Response carries a body that has already been read into memory and
// re-wrapped in an [io.NopCloser], so downstream consumers see exactly
// the same payload the wire delivered.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := rt.Base
	if base == nil {
		base = http.DefaultTransport
	}

	// Logger selection: a per-request span logger on ctx (set by the
	// crawl orchestrator via trace.WithLogger) takes precedence; this
	// is what ties HTTP lines to their owning crawl span. The
	// configured Logger is the fallback for non-crawl callers (tests,
	// one-off CLI commands). slog.Default keeps the type total.
	lg := trace.LoggerFrom(req.Context())
	if lg == nil {
		lg = rt.Logger
	}
	if lg == nil {
		lg = slog.Default()
	}
	// Tag every line emitted by this transport with trace_stream=response.
	lg = lg.With(trace.AttrTraceStream, trace.StreamResponse)

	// httptrace adds non-trivial overhead (extra context, allocation per
	// phase callback). Only wire it in when the active logger is
	// actually enabled at LevelTrace, so info-level runs pay nothing.
	var ht *httpTrace
	if lg.Enabled(req.Context(), log.LevelTrace) {
		ht = newHTTPTrace()
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), ht.clientTrace()))
		lg.LogAttrs(req.Context(), log.LevelTrace, "http.request.start",
			slog.String("http_method", req.Method),
			slog.String("url", req.URL.Path),
			slog.String("host", req.URL.Host),
			slog.Any("headers", redactHeaders(req.Header)),
		)
	}

	start := time.Now()
	resp, err := base.RoundTrip(req)
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		// Failure path: no body to read, no httptrace timings worth
		// emitting. Surface the transport error as an INFO summary
		// (writes are not failures in the level taxonomy — the caller
		// upstream decides whether the failure is "warn" or "error").
		lg.LogAttrs(req.Context(), slog.LevelInfo, "http.request.failed",
			slog.String("http_method", req.Method),
			slog.String("url", req.URL.Path),
			slog.Int64("duration_ms", durationMs),
			slog.String("error", err.Error()),
		)
		return nil, err
	}

	// Read the response body into memory so we can both log it (at
	// TRACE) and report an accurate bytes count (at INFO). The body is
	// then re-wrapped in a NopCloser so the caller sees the original
	// payload without realising we peeked. The cost is one in-memory
	// copy per response, which is acceptable for the observability
	// instrument scope.
	bodyBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	if readErr != nil {
		// Partial info is still useful. The response is still returned
		// so the caller can decide what to do; the read error is
		// recorded on the same INFO line as the rest of the summary.
		lg.LogAttrs(req.Context(), slog.LevelInfo, "http.response",
			slog.String("http_method", req.Method),
			slog.String("url", req.URL.Path),
			slog.Int("status", resp.StatusCode),
			slog.Int64("duration_ms", durationMs),
			slog.String("read_error", readErr.Error()),
		)
		return resp, nil
	}

	// INFO: the one-line measurement summary every request produces.
	lg.LogAttrs(req.Context(), slog.LevelInfo, "http.response",
		slog.String("http_method", req.Method),
		slog.String("url", req.URL.Path),
		slog.Int("status", resp.StatusCode),
		slog.Int64("duration_ms", durationMs),
		slog.Int("bytes", len(bodyBytes)),
	)

	// TRACE: full lifecycle. Only reached when httptrace was wired in
	// up-front, so the phase timings are populated.
	if ht != nil {
		lg.LogAttrs(req.Context(), log.LevelTrace, "http.request.complete",
			slog.String("http_method", req.Method),
			slog.String("url", req.URL.Path),
			slog.Int("status", resp.StatusCode),
			slog.Int64("duration_ms", durationMs),
			slog.Int("bytes", len(bodyBytes)),
			slog.Int64("dns_ms", ht.dnsMs()),
			slog.Int64("connect_ms", ht.connectMs()),
			slog.Int64("tls_ms", ht.tlsMs()),
			slog.Int64("ttfb_ms", ht.ttfbMs()),
			slog.Bool("conn_reused", ht.reused),
			slog.Any("response_headers", redactHeaders(resp.Header)),
			slog.String("response_body", string(bodyBytes)),
		)
	}

	return resp, nil
}

// ---- Redaction --------------------------------------------------------------

// redactedHeaderNames is the case-insensitive set of header names whose
// values must NEVER appear in logs, even at LevelTrace. Authorization
// carries Basic auth (the user:token base64 we mint at client.New),
// Cookie/Set-Cookie can carry session credentials, and
// X-Atlassian-Token is Jira's CSRF token.
var redactedHeaderNames = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-atlassian-token":   {},
}

// redactHeaders returns a copy of h with sensitive header values
// replaced by "REDACTED". The original header map is not mutated, so
// the request itself still carries the real credentials downstream.
// Value slices are copied so we never share the backing array with the
// caller.
func redactHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		if _, redact := redactedHeaderNames[strings.ToLower(k)]; redact {
			out[k] = []string{"REDACTED"}
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// ---- httptrace timing recorder ---------------------------------------------

// httpTrace records phase timings from net/http/httptrace. It is value-
// typed only to keep the field set tidy; the indirect pointer in
// [RoundTripper.RoundTrip] is what httptrace requires.
type httpTrace struct {
	start     time.Time
	dnsStart  time.Time
	dnsDone   time.Time
	connStart time.Time
	connDone  time.Time
	tlsStart  time.Time
	tlsDone   time.Time
	firstByte time.Time
	reused    bool
}

func newHTTPTrace() *httpTrace { return &httpTrace{start: time.Now()} }

// clientTrace returns a [*httptrace.ClientTrace] wired to record each
// phase's wall-clock boundary. Connection reuse skips the DNS / connect
// / TLS callbacks, leaving those fields zero — the *Ms helpers below
// surface -1 in that case so downstream consumers can distinguish
// "phase did not run" from "phase ran instantaneously".
func (h *httpTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart:             func(_ httptrace.DNSStartInfo) { h.dnsStart = time.Now() },
		DNSDone:              func(_ httptrace.DNSDoneInfo) { h.dnsDone = time.Now() },
		ConnectStart:         func(_, _ string) { h.connStart = time.Now() },
		ConnectDone:          func(_, _ string, _ error) { h.connDone = time.Now() },
		TLSHandshakeStart:    func() { h.tlsStart = time.Now() },
		TLSHandshakeDone:     func(_ tls.ConnectionState, _ error) { h.tlsDone = time.Now() },
		GotFirstResponseByte: func() { h.firstByte = time.Now() },
		GotConn:              func(info httptrace.GotConnInfo) { h.reused = info.Reused },
	}
}

func (h *httpTrace) dnsMs() int64 {
	if h.dnsStart.IsZero() || h.dnsDone.IsZero() {
		return -1
	}
	return h.dnsDone.Sub(h.dnsStart).Milliseconds()
}

func (h *httpTrace) connectMs() int64 {
	if h.connStart.IsZero() || h.connDone.IsZero() {
		return -1
	}
	return h.connDone.Sub(h.connStart).Milliseconds()
}

func (h *httpTrace) tlsMs() int64 {
	if h.tlsStart.IsZero() || h.tlsDone.IsZero() {
		return -1
	}
	return h.tlsDone.Sub(h.tlsStart).Milliseconds()
}

func (h *httpTrace) ttfbMs() int64 {
	if h.firstByte.IsZero() {
		return -1
	}
	return h.firstByte.Sub(h.start).Milliseconds()
}

// Compile-time assertion that *RoundTripper satisfies the interface.
var _ http.RoundTripper = (*RoundTripper)(nil)
