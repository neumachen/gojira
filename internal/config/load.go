package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/neumachen/envext"
	"github.com/neumachen/errext"
	"gopkg.in/yaml.v3"
)

// LoadOptions controls the inputs to [LoadApp]. Every field is
// optional: a zero value means "no input at that layer", and the
// cascade falls through to the next layer (or to the embedded
// defaults).
//
// CLI-flag overrides are intentionally NOT part of LoadOptions. The
// CLI is responsible for collecting flags itself, then layering them
// onto the returned [App] (Phase 5). Keeping flags out of the loader
// keeps internal/config free of cli-package dependencies.
type LoadOptions struct {
	// YAML is the (optional) raw contents of a resolved
	// gojira.yaml file. When non-nil it OVERRIDES the file
	// discovery path entirely: the resolver is not consulted,
	// ConfigPath is ignored, and the supplied reader is the
	// file layer. This shape lets programmatic callers (tests,
	// embedded services) feed an in-memory document without
	// depending on filesystem state. A nil reader means "fall
	// through to discovery". The reader is consumed once;
	// callers that need to retain the bytes should buffer them
	// externally.
	YAML io.Reader

	// ConfigPath is the (optional) explicit --config flag value
	// the CLI plumbs through to discovery. When non-empty and the
	// file does not exist, [LoadApp] treats it as a hard error
	// (the user explicitly asked for that file). Ignored entirely
	// when YAML is non-nil.
	ConfigPath string

	// Resolver is the (optional) [XDGResolver] used to perform
	// config-file discovery. When nil, [NewDefaultXDGResolver] is
	// used. Ignored entirely when YAML is non-nil. Tests inject
	// a resolver bound to t.TempDir() to drive discovery
	// deterministically without touching the real environment.
	Resolver *XDGResolver

	// Env is the (optional) environment-variable map collected
	// by the caller (typically a snapshot of os.Environ filtered
	// to the GOJIRA_* keyspace). The map is treated as read-only
	// and is alias-resolved internally; callers receive an
	// unmodified copy back via the populated App. A nil map
	// means "no env layer was supplied".
	Env map[string]string
}

// LoadApp runs the Phase 0 configuration cascade and returns a
// fully-validated [App]. The cascade order is:
//
//  1. Seed: app := DefaultApp() — embedded defaults.
//  2. Layer 1 (schema): if YAML is supplied, decode it ONCE to a
//     raw map[string]any and pass through [SchemaValidator.ValidateRaw]
//     against the embedded config.schema.json. Structural / type /
//     enum / additionalProperties failures wrap [ErrInvalidValue]
//     and short-circuit the cascade.
//  3. File: decode the SAME YAML bytes a second time, this time
//     into the [App] struct, OVER the defaults so unspecified keys
//     keep their DefaultApp() values.
//  4. Env: resolve deprecated v0.1 aliases ([ResolveAliases]), then
//     run envext over the App so GOJIRA_* environment values
//     override the file/defaults. envext leaves struct fields
//     untouched when the corresponding env key is absent or empty,
//     so file/default values survive the env pass.
//  5. Schema-version backfill: if app.Schema is still empty (env-
//     only or default-only load), set it to [SchemaVersion] so the
//     Layer-2 check below is satisfied without forcing every
//     environment-driven caller to set GOJIRA_SCHEMA.
//  6. Layer 2 (semantic): [ValidateApp] enforces required Jira
//     credentials and output directory ([ErrMissingRequired]), URL
//     parseability, enum constraints, and the schema-version pin
//     ([ErrInvalidValue]).
//
// CLI-flag overrides are NOT applied here; the CLI in Phase 5
// layers them on top of the returned App and re-validates.
func LoadApp(opts LoadOptions) (App, error) {
	app := DefaultApp()

	yamlReader, discoveredPath, err := resolveYAMLReader(opts)
	if err != nil {
		return App{}, err
	}
	if yamlReader != nil {
		defer func() {
			// Close errors on a read-only file handle that we
			// have already fully consumed are not actionable;
			// surface them through the loader's own error path
			// if they happen. The rules require checking every
			// error explicitly, hence the defer-with-anon-fn.
			if closer, ok := yamlReader.(io.Closer); ok {
				_ = closer.Close()
			}
		}()

		buf, err := io.ReadAll(yamlReader)
		if err != nil {
			return App{}, errext.WrapPrefix(err, "config: read YAML", 0)
		}
		if len(bytes.TrimSpace(buf)) > 0 {
			rawMap, err := decodeYAMLToMap(bytes.NewReader(buf))
			if err != nil {
				return App{}, err
			}
			if err := ValidateRawConfig(rawMap); err != nil {
				return App{}, err
			}
			if err := decodeYAML(bytes.NewReader(buf), &app); err != nil {
				return App{}, err
			}
		}
		// Backfill ConfigFile when discovery (not the caller-
		// supplied reader) located the file; programmatic callers
		// who pass YAML directly leave ConfigFile empty.
		if app.ConfigFile == "" && discoveredPath != "" {
			app.ConfigFile = discoveredPath
		}
	}

	if opts.Env != nil {
		envCanonical := ResolveAliases(opts.Env)
		parser, err := envext.New(envext.WithEnvMap(envCanonical))
		if err != nil {
			return App{}, errext.WrapPrefix(err, "config: build envext parser", 0)
		}
		if _, err := parser.Parse(&app); err != nil {
			return App{}, errext.WrapPrefix(err, "config: parse env", 0)
		}
	}

	if app.Schema == "" {
		app.Schema = SchemaVersion
	}

	if err := ValidateApp(&app); err != nil {
		return App{}, err
	}
	return app, nil
}

// LoadAppFromEnv is a convenience for callers that only have an env
// map (no config file). It is equivalent to
// `LoadApp(LoadOptions{Env: env})` and exists so the most common
// "parse the process environment" call site reads naturally.
func LoadAppFromEnv(env map[string]string) (App, error) {
	return LoadApp(LoadOptions{Env: env})
}

// resolveYAMLReader picks the YAML source for [LoadApp]: either the
// caller-supplied opts.YAML reader (which wins outright and skips
// discovery) or an [os.File] opened by the [XDGResolver] from a
// discovered path. The second return value is the path that was
// discovered (empty when the caller supplied YAML directly or when
// no file was found), which [LoadApp] backfills onto App.ConfigFile
// for diagnostics.
//
// The "explicit but missing" branch is the only branch that
// produces a hard error here: an opts.ConfigPath that points at a
// non-existent file is treated as a misconfiguration the user must
// see. An empty opts.ConfigPath with no file discovered anywhere is
// a successful fall-through (the loader proceeds with defaults +
// env only).
func resolveYAMLReader(opts LoadOptions) (io.Reader, string, error) {
	// Programmatic override path: a non-nil reader bypasses
	// discovery entirely so Phase 3 callers (tests, embedded
	// services) keep their exact behavior.
	if opts.YAML != nil {
		return opts.YAML, "", nil
	}

	resolver := opts.Resolver
	if resolver == nil {
		resolver = NewDefaultXDGResolver()
	}

	path, found := resolver.DiscoverConfigFile(opts.ConfigPath)
	switch {
	case found:
		// Open returns *os.File which implements io.ReadCloser.
		f, err := os.Open(path)
		if err != nil {
			return nil, "", errext.WrapPrefix(err,
				"config: open discovered config file "+path, 0)
		}
		return f, path, nil
	case path != "":
		// Discovery returned a non-empty path with found=false:
		// the user explicitly asked for opts.ConfigPath or set
		// GOJIRA_CONFIG_FILE, but the file does not exist. This
		// is a hard error, wrapping ErrInvalidValue so existing
		// errors.Is callers still classify it correctly.
		return nil, "", fmt.Errorf(
			"%w: requested config file does not exist: %s",
			ErrInvalidValue, path)
	default:
		// Nothing requested, nothing found: fall through to the
		// defaults+env layers. No error.
		return nil, "", nil
	}
}

// decodeYAML decodes a YAML document from r into into, layering on
// top of the receiver's existing field values. Unknown YAML keys
// (typos, deprecated names) are a decode error — defense in depth
// alongside the JSON-Schema additionalProperties:false constraint
// at Layer 1. A nil reader is a no-op (the App is left untouched);
// an EOF (empty document) is also a no-op so empty config files do
// not erase the defaults.
//
// Decode errors are wrapped with errext for stack traces; the
// underlying yaml.v3 error is preserved as the wrapped cause so
// errors.As(err, &yamlTypeError) still works upstream.
func decodeYAML(r io.Reader, into *App) error {
	if r == nil || into == nil {
		return nil
	}
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return errext.WrapPrefix(err, "config: decode YAML into App", 0)
	}
	return nil
}

// decodeYAMLToMap decodes the same YAML document into the loose
// map[string]any shape the JSON-Schema layer expects. The decoder
// also has KnownFields disabled (the schema's additionalProperties
// constraint catches unknown keys) so we get the structural shape
// as authored without yaml.v3 second-guessing it.
//
// A nil or empty reader returns a nil map and no error; the caller
// (LoadApp) treats that as "skip Layer 1".
func decodeYAMLToMap(r io.Reader) (map[string]any, error) {
	if r == nil {
		return nil, nil
	}
	var root any
	if err := yaml.NewDecoder(r).Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, errext.WrapPrefix(err, "config: decode YAML to map", 0)
	}
	if root == nil {
		return nil, nil
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: config root must be a YAML mapping, got %T", ErrInvalidValue, root)
	}
	return m, nil
}
