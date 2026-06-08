package config

// LogSettings configures the gojira logging subsystem. It is one of
// the four standalone entity structs composed under [App]; it can
// also be used directly by any component that only needs log config.
//
// The file is named log_settings.go (rather than log.go) so the type
// is unambiguous when read alongside the gojira "log" subpackage,
// even though they live in different Go packages.
//
// Each env key carries the full GOJIRA_LOG_* name so the parent App
// can use `env:",nested"` (empty prefix) without introducing
// nested-prefix stutter such as GOJIRA_LOG_LOG_LEVEL.
type LogSettings struct {
	// Level is the minimum log level to emit. Valid values:
	// "error", "warn", "info", "debug", "trace". Sourced from
	// GOJIRA_LOG_LEVEL or the "log.level" YAML key. "trace" is
	// gojira's own level below slog.LevelDebug (see log.LevelTrace);
	// it powers the crawl observability instrument.
	Level string `yaml:"level" json:"level" env:"GOJIRA_LOG_LEVEL"`

	// Format is the log output format. Valid values: "text"
	// (human-readable) or "json" (one JSON object per line).
	// Sourced from GOJIRA_LOG_FORMAT or the "log.format" YAML key.
	Format string `yaml:"format" json:"format" env:"GOJIRA_LOG_FORMAT"`
}

// Default log-settings constants. These mirror the flat [Config]
// defaults documented on the existing v0.1 type so [App.ToConfig]
// produces an identical Config from a DefaultApp.
const (
	DefaultLogLevel  = "info"
	DefaultLogFormat = "text"
)

// DefaultLogSettings returns the embedded log defaults: level=info,
// format=text.
func DefaultLogSettings() LogSettings {
	return LogSettings{
		Level:  DefaultLogLevel,
		Format: DefaultLogFormat,
	}
}

// EffectiveLevel returns the log level with precedence
// cli > configured > [DefaultLogLevel]. A nil receiver is tolerated
// and treated as an empty configured value.
func (l *LogSettings) EffectiveLevel(cli string) string {
	if cli != "" {
		return cli
	}
	if l != nil && l.Level != "" {
		return l.Level
	}
	return DefaultLogLevel
}

// EffectiveFormat returns the log format with precedence
// cli > configured > [DefaultLogFormat]. A nil receiver is tolerated.
func (l *LogSettings) EffectiveFormat(cli string) string {
	if cli != "" {
		return cli
	}
	if l != nil && l.Format != "" {
		return l.Format
	}
	return DefaultLogFormat
}
