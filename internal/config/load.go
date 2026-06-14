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

	// SkipSemanticValidation skips the Layer-2 semantic validator
	// (ValidateApp). Use this only for server-only config loading
	// where Jira credentials are not required. Layer-1 JSON Schema
	// validation still runs when a YAML file is present.
	SkipSemanticValidation bool
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

	// File layer. The "single source" path covers three cases that
	// must NOT layer global+local on top of each other:
	//
	//   - opts.YAML != nil (programmatic in-memory override);
	//   - opts.ConfigPath != "" (the --config flag pin);
	//   - $GOJIRA_CONFIG_FILE set (the env-var pin).
	//
	// The remaining case — pure discovery, no pin — layers the
	// global config first, then the project-local ./gojira.yaml on
	// top, so a local file overrides per FIELD instead of replacing
	// the global file wholesale.
	if err := applyFileLayer(opts, &app); err != nil {
		return App{}, err
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

	if !opts.SkipSemanticValidation {
		if err := ValidateApp(&app); err != nil {
			return App{}, err
		}
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

// LoadFileLayer runs only the file layer of the cascade and returns
// the resulting App. It does NOT apply the env layer or the Layer-2
// semantic validator, so a partial/missing-required configuration
// does NOT error out here. Layer-1 schema validation against the
// embedded JSON Schema still runs so a malformed file is caught at
// load time.
//
// LoadFileLayer is the seam the CLI uses when it wants to overlay
// env and flag values on top of the file's contribution using its
// own legacy validation path: the CLI calls LoadFileLayer, flattens
// the result, merges env + flag overrides, and runs the merged map
// through [Build]. This preserves the v0.1 *ConfigError error
// messages downstream tests and users depend on while still
// honoring the YAML file's contribution to the cascade.
//
// When configPath is empty, the resolver discovers the file (see
// [XDGResolver.DiscoverConfigFile]). When configPath is non-empty
// but the file is absent, LoadFileLayer returns an error wrapping
// [ErrInvalidValue]. When no file is discovered (no explicit path
// and no implicit candidate exists), LoadFileLayer returns
// [DefaultApp] and no error.
func LoadFileLayer(configPath string, resolver *XDGResolver) (App, error) {
	opts := LoadOptions{
		ConfigPath: configPath,
		Resolver:   resolver,
	}
	app := DefaultApp()
	if err := applyFileLayer(opts, &app); err != nil {
		return App{}, err
	}
	return app, nil
}

// applyFileLayer implements the file layer of the cascade described
// in [LoadApp]'s doc comment. The dispatch is:
//
//  1. opts.YAML != nil → decode that reader as the SOLE source. No
//     discovery, no layering. Phase 3 back-compat.
//  2. opts.ConfigPath != "" → open and decode that single file. The
//     "explicit but missing" branch is a hard error (wraps
//     [ErrInvalidValue]). No layering — the user pinned this file.
//  3. $GOJIRA_CONFIG_FILE set → same single-file pin semantics as
//     (2): the env-var-pinned file is decoded as the only source,
//     missing → hard error, no layering.
//  4. Pure discovery → decode the GLOBAL XDG config (if it exists),
//     then decode the project-local ./gojira.yaml on top (if it
//     exists). Each file goes through Layer-1 schema validation
//     independently. App.ConfigFile is set to the most-specific
//     contributing file (local > global). If neither exists this
//     is a successful no-op fall-through.
//
// The helper writes through *app in place so callers don't have to
// thread the App value through every error path.
func applyFileLayer(opts LoadOptions, app *App) error {
	// (1) Programmatic in-memory reader.
	if opts.YAML != nil {
		return applyYAMLReader(opts.YAML, "", app)
	}

	resolver := opts.Resolver
	if resolver == nil {
		resolver = NewDefaultXDGResolver()
	}

	// (2) --config flag: single pinned file.
	if opts.ConfigPath != "" {
		return applyPinnedFile(opts.ConfigPath, app)
	}

	// (3) $GOJIRA_CONFIG_FILE: single pinned file.
	if envPath, ok := resolver.ConfigFileFromEnv(); ok {
		return applyPinnedFile(envPath, app)
	}

	// (4) Discovery layering: global first, then local on top.
	// Each candidate is independently stat-checked; non-existence
	// is silent (the cascade simply falls through), in contrast to
	// the pinned-but-missing case above.
	if g := resolver.GlobalConfigFile(); g != "" && isRegularFile(g) {
		if err := applyYAMLFile(g, app); err != nil {
			return err
		}
	}
	if l := resolver.LocalConfigFile(); l != "" && isRegularFile(l) {
		if err := applyYAMLFile(l, app); err != nil {
			return err
		}
	}
	return nil
}

// applyPinnedFile opens path read-only and applies it as the SOLE
// file source, treating a missing file as the hard "explicit but
// missing" error wrapping [ErrInvalidValue]. This is the shared
// implementation for the --config flag and $GOJIRA_CONFIG_FILE
// branches of [applyFileLayer].
func applyPinnedFile(path string, app *App) error {
	if !isRegularFile(path) {
		return fmt.Errorf(
			"%w: requested config file does not exist: %s",
			ErrInvalidValue, path)
	}
	return applyYAMLFile(path, app)
}

// applyYAMLFile opens path, runs the file's contents through
// Layer-1 schema validation, then decodes it onto *app layering on
// top of any pre-existing field values. The path is backfilled onto
// app.ConfigFile (overwriting any prior value) so the most-recently
// applied file is the one reported for diagnostics — which matches
// the local-over-global layering rule (local wins, so local is the
// "owner" reported by App.ConfigFile).
//
// An empty or whitespace-only file is a no-op: defaults / prior
// layered fields are preserved. Schema-validation and decode errors
// are returned verbatim so the existing error wrapping (and
// [ErrInvalidValue] sentinel) flows out unchanged.
func applyYAMLFile(path string, app *App) error {
	f, err := os.Open(path)
	if err != nil {
		return errext.WrapPrefix(err,
			"config: open discovered config file "+path, 0)
	}
	defer func() { _ = f.Close() }()
	return applyYAMLReader(f, path, app)
}

// applyYAMLReader reads the entire YAML stream from r, validates it
// against the embedded JSON Schema (Layer 1), then decodes it onto
// *app. When discoveredPath is non-empty it is recorded on
// app.ConfigFile (overwriting any prior value) so diagnostics
// report the most-recent contributing file. An empty / whitespace-
// only stream is a no-op so empty config files do not erase
// defaults or prior layered values.
func applyYAMLReader(r io.Reader, discoveredPath string, app *App) error {
	buf, err := io.ReadAll(r)
	if err != nil {
		return errext.WrapPrefix(err, "config: read YAML", 0)
	}
	if len(bytes.TrimSpace(buf)) == 0 {
		return nil
	}
	rawMap, err := decodeYAMLToMap(bytes.NewReader(buf))
	if err != nil {
		return err
	}
	if err := ValidateRawConfig(rawMap); err != nil {
		return err
	}
	if err := decodeYAML(bytes.NewReader(buf), app); err != nil {
		return err
	}
	if discoveredPath != "" {
		app.ConfigFile = discoveredPath
	}
	return nil
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
