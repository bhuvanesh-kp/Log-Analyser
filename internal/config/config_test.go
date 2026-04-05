package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/config"
)

// ---------------------------------------------------------------------------
// Stub helper: write a YAML config to a temp file
// ---------------------------------------------------------------------------

func stubConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	return path
}

// ---------------------------------------------------------------------------
// Default values
// ---------------------------------------------------------------------------

func TestDefault(t *testing.T) {
	tests := []struct {
		desc  string
		check func(t *testing.T, cfg *config.Config)
	}{
		{
			desc: "should return 'auto' when format is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "auto", cfg.Format)
			},
		},
		{
			desc: "should return true when follow flag is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.True(t, cfg.Follow)
			},
		},
		{
			desc: "should return 100ms when poll_interval is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 100*time.Millisecond, cfg.PollInterval)
			},
		},
		{
			desc: "should return 60s when window_size is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 60*time.Second, cfg.WindowSize)
			},
		},
		{
			desc: "should return 1s when bucket_duration is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, time.Second, cfg.BucketDuration)
			},
		},
		{
			desc: "should return 3.0 when spike_multiplier is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 3.0, cfg.SpikeMultiplier)
			},
		},
		{
			desc: "should return 0.05 when error_rate_threshold is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 0.05, cfg.ErrorRateThreshold)
			},
		},
		{
			desc: "should return 0.5 when host_flood_fraction is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 0.5, cfg.HostFloodFraction)
			},
		},
		{
			desc: "should return 3.0 when latency_multiplier is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 3.0, cfg.LatencyMultiplier)
			},
		},
		{
			desc: "should return 30 when silence_threshold is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 30, cfg.SilenceThreshold)
			},
		},
		{
			desc: "should return 30s when alert_cooldown is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 30*time.Second, cfg.AlertCooldown)
			},
		},
		{
			desc: "should return 10 when min_baseline_samples is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 10, cfg.MinBaselineSamples)
			},
		},
		{
			desc: "should return 'ratio' when detection_method is not set",
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "ratio", cfg.DetectionMethod)
			},
		},
		{
			desc: "should return console alerter enabled when alerters are not configured",
			check: func(t *testing.T, cfg *config.Config) {
				assert.True(t, cfg.Alerters.Console.Enabled)
			},
		},
		{
			desc: "should return webhook alerter disabled when alerters are not configured",
			check: func(t *testing.T, cfg *config.Config) {
				assert.False(t, cfg.Alerters.Webhook.Enabled)
			},
		},
		{
			desc: "should return file alerter disabled when alerters are not configured",
			check: func(t *testing.T, cfg *config.Config) {
				assert.False(t, cfg.Alerters.File.Enabled)
			},
		},
	}

	cfg := config.Default()
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			tc.check(t, cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// YAML file loading
// ---------------------------------------------------------------------------

func TestLoadFile(t *testing.T) {
	tests := []struct {
		desc    string
		yaml    string
		check   func(t *testing.T, cfg *config.Config)
		wantErr bool
	}{
		{
			desc: "should return nginx format when format is set to nginx in file",
			yaml: `format: nginx`,
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "nginx", cfg.Format)
			},
		},
		{
			desc: "should return 30s window when window_size is set to 30s in file",
			yaml: `window_size: 30s`,
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 30*time.Second, cfg.WindowSize)
			},
		},
		{
			desc: "should return 5.0 multiplier when spike_threshold_multiplier is set to 5.0 in file",
			yaml: `spike_threshold_multiplier: 5.0`,
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 5.0, cfg.SpikeMultiplier)
			},
		},
		{
			desc: "should return console disabled when alerters.console.enabled is false in file",
			yaml: "alerters:\n  console:\n    enabled: false",
			check: func(t *testing.T, cfg *config.Config) {
				assert.False(t, cfg.Alerters.Console.Enabled)
			},
		},
		{
			desc: "should return webhook URL when alerters.webhook is configured in file",
			yaml: "alerters:\n  webhook:\n    enabled: true\n    url: \"http://example.com/hook\"",
			check: func(t *testing.T, cfg *config.Config) {
				assert.True(t, cfg.Alerters.Webhook.Enabled)
				assert.Equal(t, "http://example.com/hook", cfg.Alerters.Webhook.URL)
			},
		},
		{
			desc:    "should return error when config file path does not exist",
			yaml:    "",
			wantErr: true,
			check:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			var path string
			if tc.wantErr && tc.yaml == "" {
				path = "/nonexistent/path/config.yaml"
			} else {
				path = stubConfigFile(t, tc.yaml)
			}

			cfg, err := config.LoadFile(path)
			if tc.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// Environment variable loading
// ---------------------------------------------------------------------------

func TestLoadEnv(t *testing.T) {
	tests := []struct {
		desc    string
		envVars map[string]string
		check   func(t *testing.T, cfg *config.Config)
	}{
		{
			desc:    "should return json format when LOG_ANALYSER_FORMAT is set to json",
			envVars: map[string]string{"LOG_ANALYSER_FORMAT": "json"},
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "json", cfg.Format)
			},
		},
		{
			desc:    "should return 7.5 multiplier when LOG_ANALYSER_SPIKE_THRESHOLD_MULTIPLIER is 7.5",
			envVars: map[string]string{"LOG_ANALYSER_SPIKE_THRESHOLD_MULTIPLIER": "7.5"},
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 7.5, cfg.SpikeMultiplier)
			},
		},
		{
			desc:    "should return 120s window when LOG_ANALYSER_WINDOW_SIZE is 120s",
			envVars: map[string]string{"LOG_ANALYSER_WINDOW_SIZE": "120s"},
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 120*time.Second, cfg.WindowSize)
			},
		},
		{
			desc:    "should return webhook secret when LOG_ANALYSER_WEBHOOK_SECRET is set",
			envVars: map[string]string{"LOG_ANALYSER_WEBHOOK_SECRET": "env-secret-ok"},
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "env-secret-ok", cfg.Alerters.Webhook.Secret)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			for k, v := range tc.envVars {
				t.Setenv(k, v)
			}
			cfg, err := config.LoadEnv()
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// Config precedence
// ---------------------------------------------------------------------------

func TestLoad_Precedence(t *testing.T) {
	tests := []struct {
		desc    string
		yaml    string
		envVars map[string]string
		check   func(t *testing.T, cfg *config.Config)
	}{
		{
			desc:    "should return env value when env var and file both set the same key",
			yaml:    `format: nginx`,
			envVars: map[string]string{"LOG_ANALYSER_FORMAT": "json"},
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, "json", cfg.Format, "env var should win over file")
			},
		},
		{
			desc:    "should return file value when file overrides default and no env var is set",
			yaml:    `silence_threshold_seconds: 60`,
			envVars: nil,
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 60, cfg.SilenceThreshold, "file should win over built-in default")
			},
		},
		{
			desc:    "should return default value when neither file nor env var sets a key",
			yaml:    `format: nginx`,
			envVars: nil,
			check: func(t *testing.T, cfg *config.Config) {
				assert.Equal(t, 10, cfg.MinBaselineSamples, "default should apply when key absent from file and env")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			for k, v := range tc.envVars {
				t.Setenv(k, v)
			}
			path := stubConfigFile(t, tc.yaml)
			cfg, err := config.Load(path)
			require.NoError(t, err)
			tc.check(t, cfg)
		})
	}
}

// ---------------------------------------------------------------------------
// Security: secret in config file must be rejected
// ---------------------------------------------------------------------------

func TestSecretInFileError_Error(t *testing.T) {
	err := &config.SecretInFileError{Field: "alerters.webhook.secret"}
	msg := err.Error()
	assert.Contains(t, msg, "alerters.webhook.secret", "error message should include field name")
	assert.Contains(t, msg, "LOG_ANALYSER_ALERTERS_WEBHOOK_SECRET", "error message should suggest env var")
}

func TestLoad_RejectsSecretInFile(t *testing.T) {
	// Load() (merged file+env) should also reject secrets in file.
	yaml := "alerters:\n  webhook:\n    enabled: true\n    url: \"http://example.com\"\n    secret: \"bad\""
	_, err := config.Load(stubConfigFile(t, yaml))
	require.Error(t, err)
	assert.True(t, config.IsSecretInFileError(err), "Load should return SecretInFileError")
}

func TestLoad_ReturnsErrorOnMissingFile(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "does_not_exist.yaml"))
	assert.Error(t, err)
}

func TestLoadFile_Security(t *testing.T) {
	tests := []struct {
		desc          string
		yaml          string
		wantErr       bool
		wantSecretErr bool
	}{
		{
			desc:          "should return SecretInFileError when webhook secret is hardcoded in file",
			yaml:          "alerters:\n  webhook:\n    enabled: true\n    url: \"http://example.com\"\n    secret: \"hardcoded-bad-secret\"",
			wantErr:       true,
			wantSecretErr: true,
		},
		{
			desc:          "should return no error when webhook secret is absent from file",
			yaml:          "alerters:\n  webhook:\n    enabled: true\n    url: \"http://example.com\"",
			wantErr:       false,
			wantSecretErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := config.LoadFile(stubConfigFile(t, tc.yaml))
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, config.IsSecretInFileError(err),
					"should return SecretInFileError, got: %v", err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidate(t *testing.T) {
	tests := []struct {
		desc    string
		mutate  func(cfg *config.Config)
		wantErr bool
	}{
		{
			desc:    "should return no error when default config is validated",
			mutate:  func(cfg *config.Config) {},
			wantErr: false,
		},
		{
			desc:    "should return error when window_size is smaller than bucket_duration",
			mutate:  func(cfg *config.Config) { cfg.WindowSize = 500 * time.Millisecond },
			wantErr: true,
		},
		{
			desc:    "should return error when format is set to an unsupported value",
			mutate:  func(cfg *config.Config) { cfg.Format = "badformat" },
			wantErr: true,
		},
		{
			desc:    "should return error when detection_method is set to an unsupported value",
			mutate:  func(cfg *config.Config) { cfg.DetectionMethod = "neural-net" },
			wantErr: true,
		},
		{
			desc:    "should return error when spike_multiplier is negative",
			mutate:  func(cfg *config.Config) { cfg.SpikeMultiplier = -1.0 },
			wantErr: true,
		},
		{
			desc:    "should return error when error_rate_threshold is greater than 1",
			mutate:  func(cfg *config.Config) { cfg.ErrorRateThreshold = 1.5 },
			wantErr: true,
		},
		{
			desc:    "should return error when min_baseline_samples is zero",
			mutate:  func(cfg *config.Config) { cfg.MinBaselineSamples = 0 },
			wantErr: true,
		},
		{
			desc:    "should return error when host_flood_fraction is greater than 1",
			mutate:  func(cfg *config.Config) { cfg.HostFloodFraction = 1.5 },
			wantErr: true,
		},
		{
			desc: "should return error when webhook alerter is enabled without a URL",
			mutate: func(cfg *config.Config) {
				cfg.Alerters.Webhook.Enabled = true
				cfg.Alerters.Webhook.URL = ""
			},
			wantErr: true,
		},
		{
			desc: "should return error when file alerter is enabled without a path",
			mutate: func(cfg *config.Config) {
				cfg.Alerters.File.Enabled = true
				cfg.Alerters.File.Path = ""
			},
			wantErr: true,
		},
		{
			desc:    "should return error when latency_multiplier is zero",
			mutate:  func(cfg *config.Config) { cfg.LatencyMultiplier = 0 },
			wantErr: true,
		},
		{
			desc:    "should return error when alert_cooldown is negative",
			mutate:  func(cfg *config.Config) { cfg.AlertCooldown = -1 * time.Second },
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// configs/default.yaml round-trip
// ---------------------------------------------------------------------------

func TestDefaultYAML_LoadsAndValidates(t *testing.T) {
	// Verify that configs/default.yaml loads successfully and passes validation.
	// This catches drift between the YAML file and the Config struct.
	yamlPath := filepath.Join("..", "..", "configs", "default.yaml")
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		t.Skip("configs/default.yaml not found — skipping")
	}

	cfg, err := config.LoadFile(yamlPath)
	require.NoError(t, err, "configs/default.yaml should load without error")
	assert.NoError(t, cfg.Validate(), "configs/default.yaml should pass validation")
}

func TestDefaultYAML_MatchesDefaults(t *testing.T) {
	// Verify that key values in configs/default.yaml match config.Default().
	yamlPath := filepath.Join("..", "..", "configs", "default.yaml")
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		t.Skip("configs/default.yaml not found — skipping")
	}

	cfg, err := config.LoadFile(yamlPath)
	require.NoError(t, err)

	def := config.Default()

	assert.Equal(t, def.Format, cfg.Format, "format")
	assert.Equal(t, def.Follow, cfg.Follow, "follow")
	assert.Equal(t, def.WindowSize, cfg.WindowSize, "window_size")
	assert.Equal(t, def.BucketDuration, cfg.BucketDuration, "bucket_duration")
	assert.Equal(t, def.SpikeMultiplier, cfg.SpikeMultiplier, "spike_threshold_multiplier")
	assert.Equal(t, def.ErrorRateThreshold, cfg.ErrorRateThreshold, "error_rate_threshold")
	assert.Equal(t, def.HostFloodFraction, cfg.HostFloodFraction, "host_flood_fraction")
	assert.Equal(t, def.LatencyMultiplier, cfg.LatencyMultiplier, "latency_multiplier")
	assert.Equal(t, def.SilenceThreshold, cfg.SilenceThreshold, "silence_threshold_seconds")
	assert.Equal(t, def.AlertCooldown, cfg.AlertCooldown, "alert_cooldown")
	assert.Equal(t, def.MinBaselineSamples, cfg.MinBaselineSamples, "min_baseline_samples")
	assert.Equal(t, def.DetectionMethod, cfg.DetectionMethod, "detection_method")
	assert.Equal(t, def.Alerters.Console.Enabled, cfg.Alerters.Console.Enabled, "alerters.console.enabled")
	assert.Equal(t, def.Alerters.Webhook.Enabled, cfg.Alerters.Webhook.Enabled, "alerters.webhook.enabled")
	assert.Equal(t, def.Alerters.File.Enabled, cfg.Alerters.File.Enabled, "alerters.file.enabled")
}
