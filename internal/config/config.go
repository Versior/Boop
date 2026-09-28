// Package config loads the Boop process configuration from the environment.
package config

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Documented defaults from docs/PRODUCT.md.
const (
	DefaultAddr        = "127.0.0.1:8080"
	DefaultDataDir     = "./data"
	DefaultBaseURL     = "http://localhost:8080"
	DefaultMaxUploadMB = 10
	DefaultLogLevel    = "info"
)

const (
	minSessionSecretBytes = 32
	masterKeyBytes        = 32
	maxUploadMBCeiling    = 1024
)

// Config is the fully validated runtime configuration.
type Config struct {
	Addr          string
	DataDir       string
	BaseURL       string
	SessionSecret string
	MasterKey     string
	SecureCookies bool
	MaxUploadMB   int
	LogLevel      string
}

// Load reads and validates the BOOP_* environment variables.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

// load takes the environment lookup as a parameter so tests never depend on the
// ambient environment of the machine running them.
func load(lookup func(string) (string, bool)) (Config, error) {
	cfg := Config{
		Addr:        DefaultAddr,
		DataDir:     DefaultDataDir,
		BaseURL:     DefaultBaseURL,
		MaxUploadMB: DefaultMaxUploadMB,
		LogLevel:    DefaultLogLevel,
	}

	cfg.Addr = value(lookup, "BOOP_ADDR")
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if err := validateAddr(cfg.Addr); err != nil {
		return Config{}, fmt.Errorf("BOOP_ADDR: %w", err)
	}

	cfg.DataDir = value(lookup, "BOOP_DATA_DIR")
	if cfg.DataDir == "" {
		cfg.DataDir = DefaultDataDir
	}

	cfg.BaseURL = value(lookup, "BOOP_BASE_URL")
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	baseURL, err := normalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return Config{}, fmt.Errorf("BOOP_BASE_URL: %w", err)
	}
	cfg.BaseURL = baseURL

	cfg.SessionSecret = value(lookup, "BOOP_SESSION_SECRET")
	if n := len(cfg.SessionSecret); n > 0 && n < minSessionSecretBytes {
		return Config{}, fmt.Errorf("BOOP_SESSION_SECRET: must be at least %d bytes, got %d", minSessionSecretBytes, n)
	}

	cfg.MasterKey = value(lookup, "BOOP_MASTER_KEY")
	if cfg.MasterKey != "" {
		raw, err := base64.StdEncoding.DecodeString(cfg.MasterKey)
		if err != nil {
			return Config{}, fmt.Errorf("BOOP_MASTER_KEY: not valid base64: %w", err)
		}
		if len(raw) != masterKeyBytes {
			return Config{}, fmt.Errorf("BOOP_MASTER_KEY: must decode to %d bytes, got %d", masterKeyBytes, len(raw))
		}
	}

	if raw := value(lookup, "BOOP_SECURE_COOKIES"); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("BOOP_SECURE_COOKIES: %q is not a boolean", raw)
		}
		cfg.SecureCookies = parsed
	}

	if raw := value(lookup, "BOOP_MAX_UPLOAD_MB"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("BOOP_MAX_UPLOAD_MB: %q is not an integer", raw)
		}
		if parsed < 1 || parsed > maxUploadMBCeiling {
			return Config{}, fmt.Errorf("BOOP_MAX_UPLOAD_MB: must be between 1 and %d, got %d", maxUploadMBCeiling, parsed)
		}
		cfg.MaxUploadMB = parsed
	}

	if raw := value(lookup, "BOOP_LOG_LEVEL"); raw != "" {
		level := strings.ToLower(raw)
		switch level {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = level
		default:
			return Config{}, fmt.Errorf("BOOP_LOG_LEVEL: %q is not one of debug, info, warn, error", raw)
		}
	}

	return cfg, nil
}

// MaxUploadBytes is the request body ceiling derived from BOOP_MAX_UPLOAD_MB.
func (c Config) MaxUploadBytes() int64 {
	return int64(c.MaxUploadMB) << 20
}

// DatabasePath is the SQLite file that lives inside BOOP_DATA_DIR.
func (c Config) DatabasePath() string {
	return filepath.Join(c.DataDir, "boop.db")
}

// SlogLevel maps the validated BOOP_LOG_LEVEL value to slog.
func (c Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// value returns the trimmed variable, treating blank or missing values as unset.
func value(lookup func(string) (string, bool), key string) string {
	raw, ok := lookup(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}

func validateAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port: %w", addr, err)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil && !isHostname(host) {
			return fmt.Errorf("%q is not a valid host", host)
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not a number", port)
	}
	if number < 1 || number > 65535 {
		return fmt.Errorf("port %d is out of range", number)
	}
	return nil
}

func isHostname(host string) bool {
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, r := range label {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			default:
				return false
			}
		}
	}
	return true
}

// normalizeBaseURL requires an absolute http(s) URL rooted at the host with no
// credentials, query, fragment or sub-path: it is a canonical origin used to
// build absolute links and cookie scopes.
func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%q must use http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("%q has no host", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%q must not contain credentials", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%q must not contain a query or fragment", raw)
	}
	if path := strings.TrimSuffix(parsed.Path, "/"); path != "" {
		return "", fmt.Errorf("%q must not contain a path", raw)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}
