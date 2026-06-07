package config

import (
	"os"
	"path/filepath"
)

// XDG environment-variable names. Only XDG_CONFIG_HOME is needed for
// config-file discovery; the data/cache/state/runtime hooks defined
// by the full XDG Base Directory Specification are intentionally
// omitted — gojira's persistent state lives under the user-chosen
// output directory ([Config.OutputDir]), not under XDG_DATA_HOME.
const EnvXDGConfigHome = "XDG_CONFIG_HOME"

// EnvGojiraConfigFile is the env-var override checked between the
// explicit --config flag and the working-directory ./gojira.yaml
// candidate. It lets users (and tests) point at an out-of-tree
// config file without modifying their shell, without dropping a file
// in the cwd, and without setting XDG_CONFIG_HOME globally.
const EnvGojiraConfigFile = "GOJIRA_CONFIG_FILE"

// AppName is the directory name used inside XDG paths
// (e.g. ~/.config/gojira/config.yaml). Pinned as a const so a
// future rename of the binary is a single-line edit.
const AppName = "gojira"

// LocalConfigFileName is the YAML file searched for in the current
// working directory after the explicit and env overrides have been
// tried. It is the recommended drop-in name for per-project gojira
// configuration.
const LocalConfigFileName = "gojira.yaml"

// GlobalConfigFileName is the YAML file searched for under the
// XDG_CONFIG_HOME / ~/.config base. It is distinct from
// LocalConfigFileName because "config.yaml" is the conventional XDG
// shape ("$XDG_CONFIG_HOME/<app>/config.yaml") while "gojira.yaml"
// is the conventional repo-local shape.
const GlobalConfigFileName = "config.yaml"

// XDGResolver discovers the effective gojira configuration file using
// a small, injectable subset of the XDG Base Directory Specification.
//
// The resolver's two dependencies are both function values so tests
// can drive the discovery without touching the real environment, the
// real home directory, or any pre-existing files on the filesystem:
//
//   - lookup mirrors os.LookupEnv: (value, present).
//   - homeDir mirrors os.UserHomeDir: home directory or error.
//
// A nil value for either field on a directly-constructed XDGResolver
// is tolerated by [NewXDGResolver] (it substitutes the OS default);
// callers that construct the struct literal directly are expected to
// supply both functions themselves.
type XDGResolver struct {
	lookup  func(string) (string, bool)
	homeDir func() (string, error)
}

// NewXDGResolver returns an XDGResolver wired to the supplied
// dependencies, substituting [os.LookupEnv] / [os.UserHomeDir] for
// any nil argument. Tests pass closures over t.TempDir() to drive
// the discovery deterministically; production code uses
// [NewDefaultXDGResolver].
func NewXDGResolver(
	lookup func(string) (string, bool),
	homeDir func() (string, error),
) *XDGResolver {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	return &XDGResolver{lookup: lookup, homeDir: homeDir}
}

// NewDefaultXDGResolver returns an XDGResolver bound to the real
// process environment and user home directory. It is the resolver
// [LoadApp] picks up when LoadOptions.Resolver is nil.
func NewDefaultXDGResolver() *XDGResolver {
	return NewXDGResolver(os.LookupEnv, os.UserHomeDir)
}

// home returns the user's home directory or an empty string. When
// the home directory cannot be resolved (e.g. UserHomeDir failed in
// a sandboxed environment), an empty string is returned so the
// caller can skip the home-based candidate entirely rather than
// producing a bogus "/.config/gojira/config.yaml" path. This
// diverges from the tobi reference, which falls back to "/tmp"; for
// a config-file discovery use case, "no home" should mean "no
// home-based candidate" rather than "fabricate a candidate under
// /tmp that almost certainly won't exist".
func (r *XDGResolver) home() string {
	if r == nil {
		return ""
	}
	home, err := r.homeDir()
	if err != nil || home == "" {
		return ""
	}
	return home
}

// GlobalConfigFile returns the resolved path to the global gojira
// configuration file:
//
//   - $XDG_CONFIG_HOME/gojira/config.yaml when XDG_CONFIG_HOME is
//     set and non-empty, or
//   - ~/.config/gojira/config.yaml otherwise.
//
// When neither XDG_CONFIG_HOME nor the home directory is available,
// an empty string is returned so [DiscoverConfigFile] can skip the
// home-based candidate. The returned path is NOT guaranteed to
// exist; existence is the responsibility of the caller (or of
// [DiscoverConfigFile], which stat-checks each candidate).
func (r *XDGResolver) GlobalConfigFile() string {
	if r == nil {
		return ""
	}
	if v, ok := r.lookup(EnvXDGConfigHome); ok && v != "" {
		return filepath.Join(v, AppName, GlobalConfigFileName)
	}
	if home := r.home(); home != "" {
		return filepath.Join(home, ".config", AppName, GlobalConfigFileName)
	}
	return ""
}

// DiscoverConfigFile returns the path of the first existing
// configuration file in the documented resolution order:
//
//  1. explicit, when non-empty (the --config flag value)
//  2. $GOJIRA_CONFIG_FILE, when set and non-empty
//  3. ./gojira.yaml in the current working directory
//  4. [GlobalConfigFile] under XDG_CONFIG_HOME or ~/.config
//
// The returned (path, found) pair distinguishes three outcomes:
//
//   - ("", false): no file was found anywhere along the chain. The
//     caller treats this as a successful fall-through to the
//     defaults+env layer; it is NOT an error.
//   - (p, true): a file was found at p; the caller opens it and
//     feeds the contents to the YAML layer of the cascade.
//   - (p, false) with p != "": one of the EXPLICIT candidates
//     (--config or GOJIRA_CONFIG_FILE) was supplied but the file
//     does not exist. The caller is expected to treat this as a
//     hard error because the user asked for a specific file that
//     isn't there. The implicit candidates (3 and 4) never reach
//     this branch; they simply fall through to the next candidate.
//
// File existence is tested with [os.Stat]: a directory at the
// candidate path is NOT treated as a config file (Mode().IsRegular()
// must hold). This matches the principle of least surprise — a stray
// `gojira.yaml` directory should not be picked up as configuration.
func (r *XDGResolver) DiscoverConfigFile(explicit string) (string, bool) {
	if r == nil {
		return "", false
	}

	// 1) Explicit --config flag: pinned, no fall-through.
	if explicit != "" {
		return explicit, isRegularFile(explicit)
	}

	// 2) $GOJIRA_CONFIG_FILE: pinned, no fall-through.
	if v, ok := r.lookup(EnvGojiraConfigFile); ok && v != "" {
		return v, isRegularFile(v)
	}

	// 3) ./gojira.yaml in the working directory.
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, LocalConfigFileName)
		if isRegularFile(candidate) {
			return candidate, true
		}
	}

	// 4) Global XDG_CONFIG_HOME / ~/.config candidate.
	if g := r.GlobalConfigFile(); g != "" && isRegularFile(g) {
		return g, true
	}

	return "", false
}

// isRegularFile reports whether path exists and refers to a regular
// file. A directory, symlink-to-directory, device node, or any
// stat-error path returns false. The check is intentionally
// permissive on the read-permission front: the actual open in
// [LoadApp] is what surfaces a permission failure to the user.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
