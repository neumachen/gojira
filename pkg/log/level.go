package log

import (
	"fmt"
	"log/slog"
	"strings"
)

// gojira's intent-based level semantics (normative).
//
// The four built-in slog levels (Error / Warn / Info / Debug) are kept,
// plus the package-defined [LevelTrace] below Debug. The MEANING of each
// level is fixed across the project — it is not just "noisier" as you
// descend the ladder, it is a different kind of statement:
//
//   - error: an operation failed in a way the user must see.
//   - warn:  degraded but continuing (e.g. partial enrichment).
//   - info:  operationally significant facts AND all measurement data —
//     span boundaries worth seeing on a normal run, per-request latency
//     summaries, per-call-type tallies. A normal `info` run already
//     answers "where did the time go?".
//   - debug: durable diagnostics worth keeping even after a problem is
//     solved — resolved state and decisions (skip-if-exists hits, the
//     epic-link custom-field auto-detection result, concurrency / queue
//     state, dev-status gating).
//   - trace: traceability — the execution-flow weave. Span start/end
//     with correlation ids, the fan-out lineage ("because X has
//     relation=blocks → enqueuing Y at depth=2"), and full raw payloads.
//     Woven into the code for following execution paths; distinct from
//     `debug`'s durable-state purpose.
//
// Credential redaction is ABSOLUTE at every level, including trace. See
// the crawl-observability PRD for the full contract.

// LevelTrace is gojira's most verbose level, sitting below
// [slog.LevelDebug]. slog has no native Trace; this follows the
// conventional four-below-debug offset used by slog extensions that
// add a trace tier.
const LevelTrace slog.Level = slog.LevelDebug - 4 // -8

// ParseLevel maps a case-insensitive level name to its [slog.Level].
// Accepted: "error", "warn", "info", "debug", "trace". Leading and
// trailing whitespace is tolerated.
//
// Use this instead of [slog.Level.UnmarshalText] when reading gojira
// configuration: UnmarshalText only knows the four built-in levels and
// would reject "trace".
//
// The empty string is rejected (callers should validate non-empty
// upstream — gojira's config layer defaults LogLevel to "info" before
// it ever reaches here, so an empty value at this point indicates a
// bug worth surfacing).
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return slog.LevelError, nil
	case "warn":
		return slog.LevelWarn, nil
	case "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "trace":
		return LevelTrace, nil
	default:
		return 0, fmt.Errorf("log: invalid level %q (want error|warn|info|debug|trace)", s)
	}
}
