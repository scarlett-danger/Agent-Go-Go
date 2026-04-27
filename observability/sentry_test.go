package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestInit(t *testing.T) {
	t.Run("empty DSN is a no-op and returns false", func(t *testing.T) {
		enabled, err := Init(Config{DSN: ""})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if enabled {
			t.Error("expected enabled=false for empty DSN")
		}
	})
}

func TestFlush(t *testing.T) {
	// Flush is a no-op when Sentry is not initialised — must not panic.
	Flush(100 * time.Millisecond)
}

func TestCaptureError(t *testing.T) {
	ctx := context.Background()

	t.Run("nil error returns nil and does not panic", func(t *testing.T) {
		err := CaptureError(ctx, nil, nil, nil)
		if err != nil {
			t.Errorf("expected nil return for nil error, got %v", err)
		}
	})

	t.Run("non-nil error is returned unchanged", func(t *testing.T) {
		original := errors.New("something broke")
		returned := CaptureError(ctx, original, map[string]string{"key": "val"}, map[string]interface{}{"extra": 1})
		if returned != original {
			t.Errorf("expected same error returned, got %v", returned)
		}
	})
}

func TestCaptureMessage(t *testing.T) {
	// Must not panic regardless of Sentry init state.
	CaptureMessage(context.Background(), "test warning", sentry.LevelWarning, map[string]string{"path": "/test"})
}

func TestStartTransactionAndSpan(t *testing.T) {
	ctx := context.Background()

	txn, txnCtx := StartTransaction(ctx, "test-transaction", "test.op")
	if txn == nil {
		t.Fatal("expected non-nil transaction")
	}
	if txnCtx == nil {
		t.Fatal("expected non-nil context from StartTransaction")
	}

	span := StartSpan(txnCtx, "db.query", "SELECT * FROM messages")
	if span == nil {
		t.Fatal("expected non-nil span")
	}
	span.Finish()
	txn.Finish()
}

func TestAddBreadcrumb(t *testing.T) {
	// Must not panic with or without a hub in context.
	AddBreadcrumb(context.Background(), "slack.event", "received message", map[string]interface{}{
		"type": "message",
	})
}

func TestToLower(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Authorization", "authorization"},
		{"X-Slack-Signature", "x-slack-signature"},
		{"already-lower", "already-lower"},
		{"ALLCAPS", "allcaps"},
		{"MiXeD", "mixed"},
		{"", ""},
	}

	for _, tc := range tests {
		got := toLower(tc.input)
		if got != tc.want {
			t.Errorf("toLower(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestLooksLikeSecret(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"slack user token", "xoxp-1234567890-1234567890-abcdefghij", true},
		{"slack bot token", "xoxb-1234567890-1234567890-abcdefghij", true},
		{"slack app token", "xapp-1-A1234567890-1234567890-abcdef", true},
		{"nvapi key", "nvapi-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", true},
		{"bearer prefix", "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", true},
		{"short string not a secret", "short", false},
		{"19 chars not a secret", "1234567890123456789", false},
		{"generic long string without prefix", "this-is-a-long-string-without-known-prefix", false},
		{"empty string", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := looksLikeSecret(tc.input)
			if got != tc.want {
				t.Errorf("looksLikeSecret(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestScrubSensitive(t *testing.T) {
	t.Run("scrubs authorization header", func(t *testing.T) {
		event := &sentry.Event{
			Request: &sentry.Request{
				Headers: map[string]string{
					"Authorization":    "Bearer xoxp-real-token-here-1234567",
					"X-Slack-Signature": "v0=abc123",
					"Content-Type":    "application/json",
				},
			},
		}
		scrubSensitive(event)

		if event.Request.Headers["Authorization"] != "[scrubbed]" {
			t.Errorf("Authorization should be scrubbed, got %q", event.Request.Headers["Authorization"])
		}
		if event.Request.Headers["X-Slack-Signature"] != "[scrubbed]" {
			t.Errorf("X-Slack-Signature should be scrubbed, got %q", event.Request.Headers["X-Slack-Signature"])
		}
		if event.Request.Headers["Content-Type"] == "[scrubbed]" {
			t.Error("Content-Type should not be scrubbed")
		}
	})

	t.Run("scrubs token-like values from contexts", func(t *testing.T) {
		event := &sentry.Event{
			Contexts: map[string]sentry.Context{
				"slack": {
					"token": "xoxp-1234567890-1234567890-abcdefghij",
					"user":  "U12345",
				},
			},
		}
		scrubSensitive(event)

		if event.Contexts["slack"]["token"] != "[scrubbed]" {
			t.Errorf("token should be scrubbed, got %v", event.Contexts["slack"]["token"])
		}
		if event.Contexts["slack"]["user"] == "[scrubbed]" {
			t.Error("short user ID should not be scrubbed")
		}
	})

	t.Run("no-op on event with no request or contexts", func(t *testing.T) {
		event := &sentry.Event{}
		scrubSensitive(event) // must not panic
	})
}
