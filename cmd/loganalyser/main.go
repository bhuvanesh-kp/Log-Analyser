package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"log_analyser/internal/alerter"
	"log_analyser/internal/config"
	"log_analyser/internal/metrics"
	"log_analyser/internal/pipeline"
)

// version is set at build time via -ldflags "-X main.version=v1.0.0".
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd builds the single Cobra root command with all flags registered.
func newRootCmd() *cobra.Command {
	var cfgFile string
	var verbose bool

	cmd := &cobra.Command{
		Use:     "loganalyser",
		Short:   "Real-time log anomaly detector",
		Long:    "Tails log files, detects anomalies (rate spikes, error surges, host floods, latency spikes, silence), and delivers alerts via console, webhook, or file.",
		Version: version,
		// Silence Cobra's own error/usage printing — we handle it ourselves.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ---------------------------------------------------------------
			// 1. Load config: file + env → flag overrides → validate.
			// ---------------------------------------------------------------
			cfg, err := loadConfig(cmd, cfgFile)
			if err != nil {
				return err
			}

			// ---------------------------------------------------------------
			// 2. Logging.
			// ---------------------------------------------------------------
			setupLogging(verbose)

			// ---------------------------------------------------------------
			// 3. Alerters.
			// ---------------------------------------------------------------
			ma, err := buildAlerters(*cfg)
			if err != nil {
				return fmt.Errorf("alerter setup: %w", err)
			}

			// ---------------------------------------------------------------
			// 4. Metrics.
			// ---------------------------------------------------------------
			rec, srv := buildRecorder(*cfg)
			if srv != nil {
				go func() {
					if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						slog.Error("metrics server error", "err", err)
					}
				}()
				defer func() {
					if err := srv.Shutdown(); err != nil {
						slog.Warn("metrics server shutdown error", "err", err)
					}
				}()
				slog.Info("metrics server started", "addr", cfg.MetricsAddr)
			}

			// ---------------------------------------------------------------
			// 5. Signal handling → cancellable context.
			// ---------------------------------------------------------------
			ctx := cmd.Context()
			if ctx == nil {
				var cancel context.CancelFunc
				ctx, cancel = signal.NotifyContext(
					context.Background(),
					os.Interrupt, syscall.SIGTERM,
				)
				defer cancel()
			}

			// ---------------------------------------------------------------
			// 6. Run pipeline.
			// ---------------------------------------------------------------
			slog.Info("pipeline starting",
				"file", cfg.LogFile,
				"format", cfg.Format,
				"follow", cfg.Follow,
			)
			return pipeline.New(*cfg, ma, rec).Run(ctx)
		},
	}

	// -------------------------------------------------------------------
	// Flag registration.
	// -------------------------------------------------------------------
	f := cmd.Flags()

	// Input.
	f.StringP("file", "f", "", "Log file path (default: stdin)")
	f.BoolP("follow", "F", true, "Tail continuously")
	f.String("format", "auto", "Log format: nginx|apache|json|syslog|auto")

	// Window & detection.
	f.Duration("window", 60*time.Second, "Sliding window size")
	f.Duration("bucket", time.Second, "Bucket aggregation duration")
	f.Float64("spike-multiplier", 3.0, "Rate spike threshold (N× baseline)")
	f.Float64("error-rate", 0.05, "Error rate fraction threshold")
	f.Float64("host-flood", 0.5, "Single-IP fraction threshold")
	f.Float64("latency-multiplier", 3.0, "Latency spike multiplier")
	f.Int("silence", 30, "Consecutive zero-traffic seconds before alert")
	f.Duration("cooldown", 30*time.Second, "Per-kind alert cooldown")
	f.Int("min-baseline", 10, "Minimum window buckets before alerting")
	f.String("detection-method", "ratio", "Detection method: ratio|sigma")
	f.Int("workers", 0, "Parser worker count (0 = NumCPU)")

	// Alerters.
	f.String("webhook", "", "Webhook URL for alert delivery")
	f.String("alert-file", "", "Append JSON alerts to this file")

	// Metrics.
	f.String("metrics-addr", "", "Prometheus /metrics listen address (e.g. :9090)")

	// Config & logging.
	f.StringVarP(&cfgFile, "config", "c", "", "YAML config file path")
	f.BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")

	return cmd
}

// loadConfig loads config from file (if provided) or env-only defaults,
// then applies CLI flag overrides.
func loadConfig(cmd *cobra.Command, cfgFile string) (*config.Config, error) {
	var cfg *config.Config
	var err error

	if cfgFile != "" {
		cfg, err = config.Load(cfgFile)
		if err != nil {
			return nil, fmt.Errorf("config load: %w", err)
		}
	} else {
		cfg, err = config.LoadEnv()
		if err != nil {
			return nil, fmt.Errorf("config load env: %w", err)
		}
	}

	// Apply flag overrides — only if the flag was explicitly set by the user.
	applyFlagOverrides(cmd, cfg)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyFlagOverrides writes CLI flag values into the config, but only for
// flags that were explicitly set on the command line.
func applyFlagOverrides(cmd *cobra.Command, cfg *config.Config) {
	f := cmd.Flags()

	if f.Changed("file") {
		cfg.LogFile, _ = f.GetString("file")
	}
	if f.Changed("follow") {
		cfg.Follow, _ = f.GetBool("follow")
	}
	if f.Changed("format") {
		cfg.Format, _ = f.GetString("format")
	}
	if f.Changed("window") {
		cfg.WindowSize, _ = f.GetDuration("window")
	}
	if f.Changed("bucket") {
		cfg.BucketDuration, _ = f.GetDuration("bucket")
	}
	if f.Changed("spike-multiplier") {
		cfg.SpikeMultiplier, _ = f.GetFloat64("spike-multiplier")
	}
	if f.Changed("error-rate") {
		cfg.ErrorRateThreshold, _ = f.GetFloat64("error-rate")
	}
	if f.Changed("host-flood") {
		cfg.HostFloodFraction, _ = f.GetFloat64("host-flood")
	}
	if f.Changed("latency-multiplier") {
		cfg.LatencyMultiplier, _ = f.GetFloat64("latency-multiplier")
	}
	if f.Changed("silence") {
		cfg.SilenceThreshold, _ = f.GetInt("silence")
	}
	if f.Changed("cooldown") {
		cfg.AlertCooldown, _ = f.GetDuration("cooldown")
	}
	if f.Changed("min-baseline") {
		cfg.MinBaselineSamples, _ = f.GetInt("min-baseline")
	}
	if f.Changed("detection-method") {
		cfg.DetectionMethod, _ = f.GetString("detection-method")
	}
	if f.Changed("workers") {
		cfg.ParserWorkers, _ = f.GetInt("workers")
	}
	if f.Changed("webhook") {
		cfg.Alerters.Webhook.URL, _ = f.GetString("webhook")
	}
	if f.Changed("alert-file") {
		cfg.Alerters.File.Path, _ = f.GetString("alert-file")
	}
	if f.Changed("metrics-addr") {
		cfg.MetricsAddr, _ = f.GetString("metrics-addr")
	}
}

// setupLogging configures the global slog logger.
func setupLogging(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

// buildAlerters constructs a MultiAlerter from the config.
// ConsoleAlerter is always included. Webhook and file alerters are added
// when their respective config fields are set.
func buildAlerters(cfg config.Config) (*alerter.MultiAlerter, error) {
	var alerters []alerter.Alerter

	// Console is always enabled.
	alerters = append(alerters, alerter.NewConsoleAlerter(os.Stderr, cfg.Alerters.Console.UseColor))

	// Webhook — enabled when URL is set or Enabled is true.
	if cfg.Alerters.Webhook.URL != "" || cfg.Alerters.Webhook.Enabled {
		wh, err := alerter.NewWebhookAlerter(
			cfg.Alerters.Webhook.URL,
			cfg.Alerters.Webhook.Secret,
			&http.Client{Timeout: cfg.Alerters.Webhook.Timeout},
			cfg.Alerters.Webhook.MaxRetries,
		)
		if err != nil {
			return nil, fmt.Errorf("webhook alerter: %w", err)
		}
		alerters = append(alerters, wh)
	}

	// File — enabled when path is set.
	if cfg.Alerters.File.Path != "" {
		fa, err := alerter.NewFileAlerter(cfg.Alerters.File.Path)
		if err != nil {
			return nil, fmt.Errorf("file alerter: %w", err)
		}
		alerters = append(alerters, fa)
	}

	return alerter.NewMultiAlerter(alerters...), nil
}

// buildRecorder creates a Recorder and optionally a metrics Server.
// Returns (NoopRecorder, nil) when MetricsAddr is empty.
func buildRecorder(cfg config.Config) (metrics.Recorder, *metrics.Server) {
	if cfg.MetricsAddr == "" {
		return metrics.NoopRecorder{}, nil
	}
	rec := metrics.NewPrometheusRecorder()
	srv := metrics.NewServer(cfg.MetricsAddr, rec)
	return rec, srv
}
