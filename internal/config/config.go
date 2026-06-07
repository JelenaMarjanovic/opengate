package config

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config is the single source of truth for runtime configuration, read once at
// the composition root (System Design §23).
type Config struct {
	// slog.Level implements encoding.TextUnmarshaler, so envconfig parses
	// "DEBUG"/"INFO"/"WARN"/"ERROR". Invalid values fail Load (fail-fast).
	LogLevel slog.Level `envconfig:"LOG_LEVEL" default:"INFO"`

	// BypassRLSURL is the DSN for the BYPASSRLS operator pool (System Design
	// §10), used by the bootstrap subcommand and, from US-02.03 Step 5a, by the
	// api subcommand (pre-auth lookups and the readiness probe). It is
	// intentionally NOT required: most subcommands (e.g. migrate) do not need it,
	// so making it mandatory would break them. Each subcommand that needs it
	// (bootstrap, api) validates its presence itself.
	BypassRLSURL string `envconfig:"BYPASS_RLS_DATABASE_URL"`

	// DatabaseURL is the DSN for the regular, RLS-bound request pool (System
	// Design §22), consumed by postgres.NewPool. Like BypassRLSURL it is
	// intentionally NOT required: the commands that exist today (migrate,
	// bootstrap, and api Step 5a) do not open the regular pool, so making it
	// mandatory would break them. Its consumer is the api subcommand's
	// post-auth path, wired in US-02.03 Step 5b alongside NewPool; that step
	// validates its presence before opening the pool. NewPool itself fails fast
	// on an empty or unparseable DSN.
	DatabaseURL string `envconfig:"DATABASE_URL"`

	// HTTPAddr is the listen address for the api subcommand's HTTP server
	// (System Design §23). The default :8080 matches the §23 example and the
	// Docker Compose / Caddy edge mapping. Like the DSNs above it is not globally
	// required (it has a default and only the api subcommand reads it); the api
	// subcommand owns its validation. The env var is unprefixed to match the
	// existing config fields (LOG_LEVEL, DATABASE_URL, BYPASS_RLS_DATABASE_URL)
	// and the §23 HTTP_ADDR naming, rather than the OPENGATE_-prefixed names used
	// by the CLI-only vars read directly via os.Getenv (e.g. OPENGATE_DATABASE_URL).
	HTTPAddr string `envconfig:"HTTP_ADDR" default:":8080"`

	// CookieSecure controls the Secure attribute (and the __Host- name prefix) of
	// the session cookie the api subcommand issues. It defaults to TRUE because
	// production terminates TLS at Caddy (US-01.03) and a session cookie must be
	// HTTPS-only and __Host--prefixed there. It exists as a knob solely so the
	// integration tests, which round-trip the cookie over plain HTTP via httptest,
	// can set it FALSE — a Secure cookie is not returned by a standard client over
	// HTTP, which would break the AC-2/AC-3/AC-4 cookie round-trips. Never set it
	// false in a real deployment. See the inbound http adapter's cookie.go.
	CookieSecure bool `envconfig:"COOKIE_SECURE" default:"true"`

	// AuthzRefreshInterval is how often the api subcommand's Casbin authorizer
	// re-loads the authorization policy from casbin_rules (US-02.04). The policy is
	// migration-managed and changes rarely, so the 30s default — matching
	// authz.DefaultRefreshInterval — is ample; it is overridable via the env var for
	// integration tests (which use a short interval to observe a refresh) or
	// operational tuning. Like the DSNs and HTTP_ADDR it is read only by the api
	// subcommand. envconfig parses the duration via time.ParseDuration, so values
	// like "30s" or "1m" are accepted.
	AuthzRefreshInterval time.Duration `envconfig:"AUTHZ_REFRESH_INTERVAL" default:"30s"`
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
