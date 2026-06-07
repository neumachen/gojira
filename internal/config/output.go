package config

// OutputSettings configures where gojira writes its rendered Markdown
// output. It is one of the four standalone entity structs composed
// under [App]; it can also be used directly by any component that
// only needs the output-directory setting.
//
// The env key carries the full GOJIRA_OUTPUT_DIR name so the parent
// App can use `env:",nested"` (empty prefix) without introducing
// nested-prefix stutter such as GOJIRA_OUTPUT_OUTPUT_DIR.
type OutputSettings struct {
	// Dir is the root directory under which per-issue Markdown
	// files are written. Sourced from GOJIRA_OUTPUT_DIR or the
	// "output.dir" YAML key.
	Dir string `yaml:"dir" json:"dir" env:"GOJIRA_OUTPUT_DIR"`
}

// DefaultOutputSettings returns the zero-valued [OutputSettings].
// The output directory has no sensible embedded default; the loader
// cascade expects it to be supplied via the config file, environment,
// or CLI flags.
func DefaultOutputSettings() OutputSettings {
	return OutputSettings{}
}

// EffectiveDir returns the output directory with precedence
// cli > configured > "". A nil receiver is tolerated and treated as
// an empty configured value, mirroring the [JiraSettings] pattern.
func (o *OutputSettings) EffectiveDir(cli string) string {
	if cli != "" {
		return cli
	}
	if o != nil && o.Dir != "" {
		return o.Dir
	}
	return ""
}
