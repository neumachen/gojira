// Package buildinfo is the single source of truth for the gojira build
// identity (git revision and version ref). It is the leaf package that
// release tooling rewrites at build time, and the only package the rest of
// the codebase should consult for version reporting.
//
// # Stamping model
//
// The two consts below — commit and ref — are intentionally unexported and
// declared on their own lines so that scripts/set_version.sh can mechanically
// rewrite their values with a regex of the form:
//
//	(commit\s*=\s*")(.*)(")
//	(ref\s*=\s*")(.*)(")
//
// Because `go install github.com/neumachen/gojira/cmd/gojira@vX` compiles
// the committed source with no -ldflags, the stamped values MUST live in
// committed source for the resulting binary to report them correctly.
//
// # Import discipline
//
// This package imports ONLY the Go standard library (and, in practice, no
// stdlib packages at all). It is imported by both the root gojira facade
// (via version.go) and by pkg/client to populate the default User-Agent.
// Keeping the dependency surface empty guarantees no import cycle can be
// introduced through this package.
package buildinfo

// commit is the git SHA the binary was built from. It is rewritten by
// scripts/set_version.sh at release time. Keep this declaration on a single
// line with the literal on one line — the rewrite script targets the
// pattern `commit = "..."` exactly.
const commit = "dev"

// ref is the git tag a release was cut from, or the branch name when no tag
// applies. It is empty until rewritten by scripts/set_version.sh. Keep this
// declaration on a single line with the literal on one line — the rewrite
// script targets the pattern `ref = "..."` exactly.
const ref = ""

// Revision returns the git SHA the binary was built from, or "dev" when the
// binary has not been release-stamped.
func Revision() string {
	return commit
}

// Version returns the human-facing version string. It is the release ref
// (tag or branch) when stamped, falling back to the commit SHA otherwise.
// An un-stamped build therefore reports "dev".
func Version() string {
	return versionOf(ref, commit)
}

// FullVersion returns an image-reference-style identifier suitable for log
// lines and User-Agent strings. Format:
//
//   - stamped:    "<ref>@<commit>" (e.g. "v0.3.0@abc1234")
//   - un-stamped: "<commit>"       (e.g. "dev")
//
// The "@" separator is only emitted when both halves are present; an
// un-stamped build does not produce a bare "@dev" or "dev@".
func FullVersion() string {
	return fullVersionOf(ref, commit)
}

// UserAgent returns the HTTP User-Agent header value that the gojira HTTP
// client uses by default. Format: "gojira/" + Version().
func UserAgent() string {
	return "gojira/" + Version()
}

// versionOf is the pure helper behind Version. It is factored out so unit
// tests can exercise both the stamped and un-stamped branches without
// mutating package-level consts.
func versionOf(ref, commit string) string {
	if ref != "" {
		return ref
	}
	return commit
}

// fullVersionOf is the pure helper behind FullVersion. It is factored out
// so unit tests can exercise both the stamped and un-stamped branches
// without mutating package-level consts.
func fullVersionOf(ref, commit string) string {
	if ref != "" {
		return ref + "@" + commit
	}
	return commit
}
