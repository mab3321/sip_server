package sip

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/sip/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestCallCompletionWebhook(t *testing.T) {
	received := make(chan callCompletionPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "event-1", r.Header.Get("Idempotency-Key"))
		require.Equal(t, "service-key", r.Header.Get("X-Service-Key"))
		require.Equal(t, "Bearer bearer-token", r.Header.Get("Authorization"))
		var payload callCompletionPayload
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	want := callCompletionPayload{
		Event:           "call.completed",
		EventID:         "event-1",
		CallID:          "SCL_test",
		DurationSeconds: 12.5,
	}
	sendCallCompletionWebhook(logger.GetLogger(), &config.CallCompletionWebhookConfig{
		URL:          srv.URL,
		BearerToken:  "bearer-token",
		XServiceKey:  "service-key",
		Timeout:      time.Second,
		MaxAttempts:  1,
		RetryBackoff: time.Millisecond,
	}, want)

	select {
	case got := <-received:
		require.Equal(t, want.Event, got.Event)
		require.Equal(t, want.EventID, got.EventID)
		require.Equal(t, want.CallID, got.CallID)
		require.Equal(t, want.DurationSeconds, got.DurationSeconds)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for call completion webhook")
	}
}

func TestElapsedSeconds(t *testing.T) {
	start := time.Unix(100, 0)
	require.Equal(t, 2.5, elapsedSeconds(start, start.Add(2500*time.Millisecond)))
	require.Zero(t, elapsedSeconds(time.Time{}, start))
	require.Zero(t, elapsedSeconds(start, start.Add(-time.Second)))
}
