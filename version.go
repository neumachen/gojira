package gojira

import "github.com/neumachen/gojira/internal/buildinfo"

// This file re-exports the build identity from internal/buildinfo at the
// module facade. External callers should use these accessors (rather than
// importing internal/buildinfo, which is not part of the public API) so
// release tooling has a single rewrite target.

// Revision returns the git SHA the binary was built from, or "dev" when
// the binary has not been release-stamped. It is a thin wrapper over
// [buildinfo.Revision].
func Revision() string {
	return buildinfo.Revision()
}

// Version returns the human-facing version string: the release ref (tag or
// branch) when stamped, falling back to the commit SHA otherwise. An
// un-stamped build therefore reports "dev". It is a thin wrapper over
// [buildinfo.Version].
//
// Note: in v0.3 and earlier, Version was a package-level CONST. It is now a
// FUNC so the value can be set by release tooling at build time without
// requiring -ldflags (which `go install ...@vX` does not pass). Update
// callers from `gojira.Version` to `gojira.Version()`.
func Version() string {
	return buildinfo.Version()
}

// FullVersion returns an image-reference-style identifier suitable for log
// lines and diagnostic banners. It is "<ref>@<commit>" on a stamped build
// and just "<commit>" otherwise. It is a thin wrapper over
// [buildinfo.FullVersion].
func FullVersion() string {
	return buildinfo.FullVersion()
}

// UserAgent returns the default HTTP User-Agent header value the gojira
// HTTP client sends ("gojira/" + Version()). It is a thin wrapper over
// [buildinfo.UserAgent].
func UserAgent() string {
	return buildinfo.UserAgent()
}
