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

	"boop/internal/media"
	"boop/internal/secretbox"
	"boop/internal/settings"
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
	// Storage is the object-store half of the upload configuration. It stays
	// zero in the default deployment, where uploads are files under DataDir.
	Storage Storage
}

// Storage is the S3-compatible bucket uploads can be kept in instead of the
// local directory. docs/PRODUCT.md §6 puts the storage location under
// environment control rather than the settings page, because a path or a bucket
// is a deployment fact and a wrong one is not recoverable from a web form.
type Storage struct {
	Endpoint  string
	Region    string
	Bucket    string
	Prefix    string
	PublicURL string
	AccessKey string
	SecretKey string
}

// ObjectStorage maps the environment variables onto the media layer's own
// description of a bucket, so the shape of the configuration is validated by
// the code that has to use it.
func (c Config) ObjectStorage() media.ObjectOptions {
	return media.ObjectOptions{
		Endpoint:  c.Storage.Endpoint,
		Region:    c.Storage.Region,
		Bucket:    c.Storage.Bucket,
		Prefix:    c.Storage.Prefix,
		PublicURL: c.Storage.PublicURL,
		AccessKey: c.Storage.AccessKey,
		SecretKey: c.Storage.SecretKey,
	}
}

// StorageSelection maps the environment onto the settings package's own
// description of where uploads are kept. The same value seeds the settings
// table on a first start, is reported at startup, and stays in charge when a
// site has never saved the storage category — so the mapping exists once
// rather than in each of those three places.
func (c Config) StorageSelection() settings.Storage {
	object := c.ObjectStorage()
	if !object.Enabled() {
		return settings.Storage{Mode: settings.StorageModeLocal}
	}
	return settings.Storage{
		Mode:            settings.StorageModeObject,
		Endpoint:        object.Endpoint,
		Region:          object.Region,
		Bucket:          object.Bucket,
		Prefix:          object.Prefix,
		PublicURL:       object.PublicURL,
		AccessKeyID:     object.AccessKey,
		SecretAccessKey: object.SecretKey,
	}
}

// storageVariables names every object-storage variable next to the value it
// fills, so the read and the "what is missing" check cannot drift apart.
func (c Config) storageVariables() [][2]string {
	return [][2]string{
		{"BOOP_R2_ENDPOINT", c.Storage.Endpoint},
		{"BOOP_R2_BUCKET", c.Storage.Bucket},
		{"BOOP_R2_ACCESS_KEY_ID", c.Storage.AccessKey},
		{"BOOP_R2_SECRET_ACCESS_KEY", c.Storage.SecretKey},
		{"BOOP_R2_PUBLIC_URL", c.Storage.PublicURL},
	}
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

	cfg.Storage = Storage{
		Endpoint: value(lookup, "BOOP_R2_ENDPOINT"),
		Region:   value(lookup, "BOOP_R2_REGION"),
		Bucket:   value(lookup, "BOOP_R2_BUCKET"),
		// A prefix is written as a path in documentation and stored without its
		// slashes, so both /uploads and uploads/ mean the same key namespace.
		Prefix:    strings.Trim(value(lookup, "BOOP_R2_PREFIX"), "/"),
		PublicURL: value(lookup, "BOOP_R2_PUBLIC_URL"),
		AccessKey: value(lookup, "BOOP_R2_ACCESS_KEY_ID"),
		SecretKey: value(lookup, "BOOP_R2_SECRET_ACCESS_KEY"),
	}
	if err := validateStorage(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validateStorage accepts an absent object store and refuses a half-configured
// one: the process stops at startup rather than at the first upload of the day,
// and the message names the variable that is missing.
func validateStorage(cfg Config) error {
	object := cfg.ObjectStorage()
	if !object.Enabled() {
		return nil
	}
	var missing []string
	for _, variable := range cfg.storageVariables() {
		if variable[1] == "" {
			missing = append(missing, variable[0])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("object storage is configured but incomplete, missing %s", strings.Join(missing, ", "))
	}
	if err := object.Validate(); err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	return nil
}

// SecretBox is the box that encrypts every stored secret, or nil when
// BOOP_MASTER_KEY is absent. The box is optional on purpose: a nil one makes
// every secret feature fail closed instead of storing plaintext.
func (c Config) SecretBox() (*secretbox.Box, error) {
	if c.MasterKey == "" {
		return nil, nil
	}
	return secretbox.NewFromBase64(c.MasterKey)
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
