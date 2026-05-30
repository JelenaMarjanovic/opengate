package config

import (
	"fmt"
	"log/slog"

	"github.com/kelseyhightower/envconfig"
)

// Config is the single source of truth for runtime configuration, read once at
// the composition root (System Design §23).
type Config struct {
	// slog.Level implements encoding.TextUnmarshaler, so envconfig parses
	// "DEBUG"/"INFO"/"WARN"/"ERROR". Invalid values fail Load (fail-fast).
	LogLevel slog.Level `envconfig:"LOG_LEVEL" default:"INFO"`

	// BypassRLSURL is the DSN for the BYPASSRLS operator pool (System Design
	// §10), used by the bootstrap subcommand. It is intentionally NOT required:
	// most subcommands (e.g. migrate) do not need it, so making it mandatory
	// would break them. The bootstrap subcommand validates its presence itself.
	BypassRLSURL string `envconfig:"BYPASS_RLS_DATABASE_URL"`
}

// Load reads configuration from the environment. A parse failure returns an
// error so the caller exits immediately; config errors are programmer errors.
func Load() (Config, error) {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return Config{}, fmt.Errorf("load config: %w", err)
	}
	return cfg, nil
}
