package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// ---------------------------------------------------------------------------
// Error types
// ---------------------------------------------------------------------------

// SecretInFileError is returned when a secret field is found hardcoded in a
// config file. Secrets must be supplied via environment variables only.
type SecretInFileError struct {
	Field string
}

func (e *SecretInFileError) Error() string {
	envKey := "LOG_ANALYSER_" + strings.ToUpper(
		strings.NewReplacer(".", "_", "-", "_").Replace(e.Field),
	)
	return fmt.Sprintf(
		"config: secret field %q must not be stored in config file; use %s env var instead",
		e.Field, envKey,
	)
}

// IsSecretInFileError reports whether err is, or wraps, a *SecretInFileError.
func IsSecretInFileError(err error) bool {
	var target *SecretInFileError
	return errors.As(err, &target)
}

// ---------------------------------------------------------------------------
// Sub-config structs
// ---------------------------------------------------------------------------

// ConsoleConfig controls the coloured terminal alerter.
type ConsoleConfig struct {
	Enabled    bool   `yaml:"enabled"`
	UseColor   bool   `yaml:"use_color"`
	TimeFormat string `yaml:"time_format"`
}

// WebhookConfig controls the HTTP POST JSON alerter.
// Secret must never appear in a config file — use LOG_ANALYSER_WEBHOOK_SECRET.
type WebhookConfig struct {
	Enabled    bool          `yaml:"enabled"`
	URL        string        `yaml:"url"`
	Secret     string        `yaml:"secret"`
	Timeout    time.Duration `yaml:"timeout"`
	MaxRetries int           `yaml:"max_retries"`
	RetryDelay time.Duration `yaml:"retry_delay"`
}

// FileConfig controls the append-JSON file alerter.
type FileConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Path      string `yaml:"path"`
	Format    string `yaml:"format"`
	MaxSizeMB int    `yaml:"max_size_mb"`
}

// AlertersConfig groups all alerter configurations.
type AlertersConfig struct {
	Console ConsoleConfig `yaml:"console"`
	Webhook WebhookConfig `yaml:"webhook"`
	File    FileConfig    `yaml:"file"`
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Config holds the complete runtime configuration for log_analyser.
type Config struct {
	LogFile            string         `yaml:"log_file"`
	Format             string         `yaml:"format"`
	Follow             bool           `yaml:"follow"`
	PollInterval       time.Duration  `yaml:"poll_interval"`
	WindowSize         time.Duration  `yaml:"window_size"`
	BucketDuration     time.Duration  `yaml:"bucket_duration"`
	SpikeMultiplier    float64        `yaml:"spike_threshold_multiplier"`
	ErrorRateThreshold float64        `yaml:"error_rate_threshold"`
	HostFloodFraction  float64        `yaml:"host_flood_fraction"`
	LatencyMultiplier  float64        `yaml:"latency_multiplier"`
	SilenceThreshold   int            `yaml:"silence_threshold_seconds"`
	AlertCooldown      time.Duration  `yaml:"alert_cooldown"`
	MinBaselineSamples int            `yaml:"min_baseline_samples"`
	DetectionMethod    string         `yaml:"detection_method"`
	ParserWorkers      int            `yaml:"parser_workers"`
	MetricsAddr        string         `yaml:"metrics_addr"`
	Alerters           AlertersConfig `yaml:"alerters"`
}

// ---------------------------------------------------------------------------
// Default
// ---------------------------------------------------------------------------

// Default returns a Config populated with production-safe built-in defaults.
func Default() *Config {
	return &Config{
		Format:             "auto",
		Follow:             true,
		PollInterval:       100 * time.Millisecond,
		WindowSize:         60 * time.Second,
		BucketDuration:     time.Second,
		SpikeMultiplier:    3.0,
		ErrorRateThreshold: 0.05,
		HostFloodFraction:  0.5,
		LatencyMultiplier:  3.0,
		SilenceThreshold:   30,
		AlertCooldown:      30 * time.Second,
		MinBaselineSamples: 10,
		DetectionMethod:    "ratio",
		Alerters: AlertersConfig{
			Console: ConsoleConfig{
				Enabled:    true,
				UseColor:   true,
				TimeFormat: "15:04:05",
			},
			Webhook: WebhookConfig{
				Enabled:    false,
				Timeout:    5 * time.Second,
				MaxRetries: 3,
				RetryDelay: 2 * time.Second,
			},
			File: FileConfig{
				Enabled:   false,
				Format:    "json",
				MaxSizeMB: 50,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Viper helpers
// ---------------------------------------------------------------------------

// newViper creates a Viper instance pre-loaded with all built-in defaults.
func newViper() *viper.Viper {
	v := viper.New()
	v.SetDefault("format", "auto")
	v.SetDefault("follow", true)
	v.SetDefault("poll_interval", "100ms")
	v.SetDefault("window_size", "60s")
	v.SetDefault("bucket_duration", "1s")
	v.SetDefault("spike_threshold_multiplier", 3.0)
	v.SetDefault("error_rate_threshold", 0.05)
	v.SetDefault("host_flood_fraction", 0.5)
	v.SetDefault("latency_multiplier", 3.0)
	v.SetDefault("silence_threshold_seconds", 30)
	v.SetDefault("alert_cooldown", "30s")
	v.SetDefault("min_baseline_samples", 10)
	v.SetDefault("detection_method", "ratio")
	v.SetDefault("alerters.console.enabled", true)
	v.SetDefault("alerters.webhook.enabled", false)
	v.SetDefault("alerters.file.enabled", false)
	return v
}

// setupEnv binds the LOG_ANALYSER_* environment variables to viper keys.
// AutomaticEnv handles simple top-level keys (e.g. LOG_ANALYSER_FORMAT).
// Nested alerter keys need explicit BindEnv because the env-var convention
// omits the "alerters" prefix segment.
func setupEnv(v *viper.Viper) {
	v.SetEnvPrefix("LOG_ANALYSER")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	_ = v.BindEnv("alerters.webhook.secret", "LOG_ANALYSER_WEBHOOK_SECRET")
	_ = v.BindEnv("alerters.webhook.url", "LOG_ANALYSER_WEBHOOK_URL")
	_ = v.BindEnv("alerters.webhook.enabled", "LOG_ANALYSER_WEBHOOK_ENABLED")
	_ = v.BindEnv("alerters.console.enabled", "LOG_ANALYSER_CONSOLE_ENABLED")
	_ = v.BindEnv("alerters.file.enabled", "LOG_ANALYSER_FILE_ENABLED")
	_ = v.BindEnv("alerters.file.path", "LOG_ANALYSER_FILE_PATH")
}

// extract maps viper keys into a Config struct.
func extract(v *viper.Viper) *Config {
	return &Config{
		LogFile:            v.GetString("log_file"),
		Format:             v.GetString("format"),
		Follow:             v.GetBool("follow"),
		PollInterval:       v.GetDuration("poll_interval"),
		WindowSize:         v.GetDuration("window_size"),
		BucketDuration:     v.GetDuration("bucket_duration"),
		SpikeMultiplier:    v.GetFloat64("spike_threshold_multiplier"),
		ErrorRateThreshold: v.GetFloat64("error_rate_threshold"),
		HostFloodFraction:  v.GetFloat64("host_flood_fraction"),
		LatencyMultiplier:  v.GetFloat64("latency_multiplier"),
		SilenceThreshold:   v.GetInt("silence_threshold_seconds"),
		AlertCooldown:      v.GetDuration("alert_cooldown"),
		MinBaselineSamples: v.GetInt("min_baseline_samples"),
		DetectionMethod:    v.GetString("detection_method"),
		MetricsAddr:        v.GetString("metrics_addr"),
		Alerters: AlertersConfig{
			Console: ConsoleConfig{
				Enabled:  v.GetBool("alerters.console.enabled"),
				UseColor: v.GetBool("alerters.console.use_color"),
			},
			Webhook: WebhookConfig{
				Enabled: v.GetBool("alerters.webhook.enabled"),
				URL:     v.GetString("alerters.webhook.url"),
				Secret:  v.GetString("alerters.webhook.secret"),
			},
			File: FileConfig{
				Enabled: v.GetBool("alerters.file.enabled"),
				Path:    v.GetString("alerters.file.path"),
			},
		},
	}
}

// checkForFileSecrets returns SecretInFileError if any secret field is
// populated in the loaded file (checked before env vars are applied, so
// any non-empty value here came from the file itself).
func checkForFileSecrets(v *viper.Viper) error {
	if v.GetString("alerters.webhook.secret") != "" {
		return &SecretInFileError{Field: "alerters.webhook.secret"}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Public loaders
// ---------------------------------------------------------------------------

// LoadFile loads configuration from a YAML file only (no env var merging).
// Returns SecretInFileError if a secret field is found in the file.
func LoadFile(path string) (*Config, error) {
	v := newViper()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	if err := checkForFileSecrets(v); err != nil {
		return nil, err
	}
	return extract(v), nil
}

// LoadEnv loads configuration from environment variables only,
// falling back to built-in defaults when a variable is not set.
func LoadEnv() (*Config, error) {
	v := newViper()
	setupEnv(v)
	return extract(v), nil
}

// Load merges a YAML config file with environment variable overrides.
// Precedence (highest → lowest): env vars > file values > built-in defaults.
// Returns SecretInFileError if a secret field is found in the file.
func Load(path string) (*Config, error) {
	v := newViper()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}
	if err := checkForFileSecrets(v); err != nil {
		return nil, err
	}
	setupEnv(v)
	return extract(v), nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

var (
	validFormats = map[string]bool{
		"auto": true, "nginx": true, "apache": true, "json": true, "syslog": true,
	}
	validDetectionMethods = map[string]bool{
		"ratio": true, "sigma": true,
	}
)

// Validate checks all Config fields are within acceptable ranges and that
// dependent fields are consistent. Call this after loading, before the
// pipeline starts.
func (c *Config) Validate() error {
	if !validFormats[c.Format] {
		return fmt.Errorf("config: invalid format %q; must be one of: auto, nginx, apache, json, syslog", c.Format)
	}
	if !validDetectionMethods[c.DetectionMethod] {
		return fmt.Errorf("config: invalid detection_method %q; must be ratio or sigma", c.DetectionMethod)
	}
	if c.WindowSize < c.BucketDuration {
		return fmt.Errorf("config: window_size (%v) must be >= bucket_duration (%v)", c.WindowSize, c.BucketDuration)
	}
	if c.SpikeMultiplier <= 0 {
		return fmt.Errorf("config: spike_multiplier must be > 0, got %v", c.SpikeMultiplier)
	}
	if c.ErrorRateThreshold <= 0 || c.ErrorRateThreshold > 1 {
		return fmt.Errorf("config: error_rate_threshold must be in (0, 1], got %v", c.ErrorRateThreshold)
	}
	if c.HostFloodFraction <= 0 || c.HostFloodFraction > 1 {
		return fmt.Errorf("config: host_flood_fraction must be in (0, 1], got %v", c.HostFloodFraction)
	}
	if c.LatencyMultiplier <= 0 {
		return fmt.Errorf("config: latency_multiplier must be > 0, got %v", c.LatencyMultiplier)
	}
	if c.MinBaselineSamples <= 0 {
		return fmt.Errorf("config: min_baseline_samples must be > 0, got %v", c.MinBaselineSamples)
	}
	if c.AlertCooldown < 0 {
		return fmt.Errorf("config: alert_cooldown must be >= 0, got %v", c.AlertCooldown)
	}
	if c.Alerters.Webhook.Enabled && c.Alerters.Webhook.URL == "" {
		return fmt.Errorf("config: alerters.webhook is enabled but url is empty")
	}
	if c.Alerters.File.Enabled && c.Alerters.File.Path == "" {
		return fmt.Errorf("config: alerters.file is enabled but path is empty")
	}
	return nil
}
