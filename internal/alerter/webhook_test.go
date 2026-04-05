package alerter_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"log_analyser/internal/alerter"
	"log_analyser/internal/analyzer"
)

// newTestServer creates a test HTTP server and registers a cleanup to close it.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestWebhookAlerter_Name(t *testing.T) {
	a, err := alerter.NewWebhookAlerter("http://example.com", "", nil, 1)
	require.NoError(t, err)
	assert.Equal(t, "webhook", a.Name())
}

func TestWebhookAlerter_New_ErrorOnEmptyURL(t *testing.T) {
	_, err := alerter.NewWebhookAlerter("", "", nil, 1)
	assert.Error(t, err)
}

func TestWebhookAlerter_Send_PostsToURL(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		called <- struct{}{}
		w.WriteHeader(http.StatusOK)
	})

	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("server did not receive a request")
	}
}

func TestWebhookAlerter_Send_ContentTypeJSON(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_AlertIDHeaderPresent(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get("X-Alert-ID"), "X-Alert-ID header should be set")
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_AlertIDInBody(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		assert.NotEmpty(t, payload["alert_id"], "JSON body should contain alert_id field")
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_SignatureHeaderPresentWhenSecretSet(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get("X-Signature-SHA256"), "signature header should be set when secret is configured")
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "test-secret", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_NoSignatureHeaderWhenNoSecret(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("X-Signature-SHA256"), "signature header should be absent when no secret")
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_SignatureMatchesHMAC(t *testing.T) {
	const secret = "signing-secret"
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		assert.Equal(t, expected, r.Header.Get("X-Signature-SHA256"))
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, secret, srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_BodyContainsKind(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(body, &payload))
		assert.Equal(t, "rate_spike", payload["kind"])
		w.WriteHeader(http.StatusOK)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1)
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
}

func TestWebhookAlerter_Send_RetriesOn5xx(t *testing.T) {
	attempts := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	})
	// Zero delays so the test doesn't sleep.
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 3,
		alerter.WithRetryDelays([]time.Duration{0, 0}))
	require.NoError(t, err)
	require.NoError(t, a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning)))
	assert.Equal(t, 3, attempts, "should retry until success on 5xx")
}

func TestWebhookAlerter_Send_NoRetryOn4xx(t *testing.T) {
	attempts := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 3,
		alerter.WithRetryDelays([]time.Duration{0, 0}))
	require.NoError(t, err)
	err = a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	assert.Error(t, err)
	assert.Equal(t, 1, attempts, "should not retry on 4xx")
}

func TestWebhookAlerter_Send_ErrorMessageContainsStatusCode(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 1,
		alerter.WithRetryDelays([]time.Duration{0}))
	require.NoError(t, err)
	err = a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "418", "error should include the HTTP status code")
	assert.Contains(t, err.Error(), "webhook", "error should identify webhook")
}

func TestWebhookAlerter_Send_ReturnsErrorAfterMaxRetries(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 3,
		alerter.WithRetryDelays([]time.Duration{0, 0}))
	require.NoError(t, err)
	err = a.Send(context.Background(), testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	assert.Error(t, err, "should return error when all retries exhausted")
}

func TestWebhookAlerter_Send_AbortsOnContextCancel(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // always fail to trigger retries
	})
	// Long delays so the context timeout fires during a backoff sleep.
	a, err := alerter.NewWebhookAlerter(srv.URL, "", srv.Client(), 10,
		alerter.WithRetryDelays([]time.Duration{2 * time.Second, 2 * time.Second}))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = a.Send(ctx, testAnomaly(analyzer.KindRateSpike, analyzer.SeverityWarning))
	elapsed := time.Since(start)

	assert.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second, "should abort retries when context is cancelled")
}
