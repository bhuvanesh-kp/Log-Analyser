package alerter

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"log_analyser/internal/analyzer"
)

// WebhookOption configures a WebhookAlerter.
type WebhookOption func(*WebhookAlerter)

// WithRetryDelays overrides the inter-attempt backoff durations.
// The i-th element is the sleep before the (i+1)-th attempt.
// Useful in tests to set zero-duration delays.
func WithRetryDelays(delays []time.Duration) WebhookOption {
	return func(w *WebhookAlerter) {
		w.retryDelays = delays
	}
}

// WebhookAlerter POSTs anomaly JSON to an HTTP endpoint with optional
// HMAC-SHA256 signing and exponential-backoff retry.
type WebhookAlerter struct {
	url         string
	secret      string
	client      *http.Client
	maxRetries  int
	retryDelays []time.Duration // delays[i] = sleep before attempt i+1
}

// NewWebhookAlerter creates a WebhookAlerter.
// client may be nil (a default 5-second-timeout client is used).
// maxRetries is the total number of attempts (including the first).
func NewWebhookAlerter(url, secret string, client *http.Client, maxRetries int, opts ...WebhookOption) (*WebhookAlerter, error) {
	if url == "" {
		return nil, errors.New("webhook URL must not be empty")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	wa := &WebhookAlerter{
		url:         url,
		secret:      secret,
		client:      client,
		maxRetries:  maxRetries,
		retryDelays: []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
	}
	for _, opt := range opts {
		opt(wa)
	}
	return wa, nil
}

func (w *WebhookAlerter) Name() string { return "webhook" }

// Send marshals the anomaly, then attempts delivery up to maxRetries times.
// Retries on 5xx / network errors; aborts immediately on 4xx or ctx cancellation.
func (w *WebhookAlerter) Send(ctx context.Context, a analyzer.Anomaly) error {
	alertID := uuid.New().String()

	payload := struct {
		AlertID string `json:"alert_id"`
		analyzer.Anomaly
	}{AlertID: alertID, Anomaly: a}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < w.maxRetries; attempt++ {
		// Sleep before every retry (not before the first attempt).
		if attempt > 0 {
			idx := attempt - 1
			if idx >= len(w.retryDelays) {
				idx = len(w.retryDelays) - 1
			}
			delay := w.retryDelays[idx]
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return ctx.Err()
				}
			} else {
				// Zero delay — still honour cancellation.
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
		}

		lastErr = w.do(ctx, alertID, body)
		if lastErr == nil {
			return nil
		}

		// Propagate context errors immediately.
		if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
			return lastErr
		}

		// Do not retry client errors (4xx).
		var statusErr *httpStatusError
		if errors.As(lastErr, &statusErr) && statusErr.code >= 400 && statusErr.code < 500 {
			return lastErr
		}
	}
	return lastErr
}

// do performs a single HTTP POST with signing headers.
func (w *WebhookAlerter) do(ctx context.Context, alertID string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Alert-ID", alertID)

	if w.secret != "" {
		mac := hmac.New(sha256.New, []byte(w.secret))
		mac.Write(body)
		req.Header.Set("X-Signature-SHA256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return &httpStatusError{code: resp.StatusCode}
	}
	return nil
}

// httpStatusError carries a non-2xx/3xx HTTP status code.
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("webhook returned HTTP %d", e.code) }
