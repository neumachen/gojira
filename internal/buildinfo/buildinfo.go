// Package buildinfo is the single source of truth for the gojira build
// identity (git revision and version ref). It combines two sources:
//
//  1. CD stamping: scripts/set_version.sh rewrites the commit/ref consts
//     transiently in the Docker build context.
//  2. Go build info: runtime/debug.ReadBuildInfo() provides module version
//     and VCS metadata when installed via `go install ...@vX.Y.Z`.
//
// The functions in this package check CD-stamped values first, then fall
// back to ReadBuildInfo(), then to "dev" for local development builds.
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
// CD stamps these transiently in the build context; committed source always
// has "dev" and "" respectively.
//
// # Import discipline
//
// This package imports only runtime/debug from the standard library. It is
// imported by both the root gojira facade (via version.go) and by pkg/client
// to populate the default User-Agent. Keeping the dependency surface minimal
// guarantees no import cycle can be introduced through this package.
package buildinfo

import "runtime/debug"

// commit is the git SHA the binary was built from. It is rewritten by
// scripts/set_version.sh in the CD build context only. Keep this declaration
// on a single line — the rewrite script targets the pattern `commit = "..."`
// exactly. Committed source always has "dev".
const commit = "dev"

// ref is the git tag or branch name. It is rewritten by scripts/set_version.sh
// in the CD build context only. Keep this declaration on a single line — the
// rewrite script targets the pattern `ref = "..."` exactly. Committed source
// always has "".
const ref = ""

// Revision returns the git SHA the binary was built from.
//
// Resolution order:
//  1. CD-stamped commit const (if not "dev")
//  2. vcs.revision from debug.ReadBuildInfo() (go install / go build with VCS)
//  3. "dev" fallback for local builds
func Revision() string {
	// CD-stamped value takes precedence
	if commit != "dev" && commit != "" {
		return commit
	}
	// Try build info (go install or go build with VCS)
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "dev"
}

// Version returns the human-facing version string.
//
// Resolution order:
//  1. CD-stamped ref const (if not empty)
//  2. Main.Version from debug.ReadBuildInfo() (go install ...@vX.Y.Z)
//  3. CD-stamped commit (if not "dev")
//  4. "dev" fallback for local builds
func Version() string {
	// CD-stamped ref takes precedence
	if ref != "" {
		return ref
	}
	// Try build info (go install ...@vX.Y.Z)
	if info, ok := debug.ReadBuildInfo(); ok {
		v := info.Main.Version
		if v != "" && v != "(devel)" {
			return v
		}
	}
	// Fall back to commit if stamped
	if commit != "dev" && commit != "" {
		return commit
	}
	return "dev"
}

// FullVersion returns an image-reference-style identifier suitable for log
// lines and diagnostic output. Format:
//
//   - When version and revision differ: "<version>@<revision>" (e.g. "v0.4.1@abc1234")
//   - Otherwise: "<version>" (e.g. "dev" or "v0.4.1")
func FullVersion() string {
	v := Version()
	r := Revision()
	// Combine if both are meaningful and different
	if v != "dev" && r != "dev" && v != r {
		return v + "@" + r
	}
	return v
}

// UserAgent returns the HTTP User-Agent header value that the gojira HTTP
// client uses by default. Format: "gojira/" + Version().
func UserAgent() string {
	return "gojira/" + Version()
}
