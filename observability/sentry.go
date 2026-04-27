package observability

import (
	"context"

	"fmt"

	"log"

	"time"

	"github.com/getsentry/sentry-go"
)

// Config carries the Sentry settings extracted from env vars.

type Config struct {
	DSN string // Empty = telemetry disabled (no-op mode)

	Environment string // e.g. "development", "production"

	Release string // git sha or semver — helps Sentry group regressions

	TracesSampleRate float64 // 0.0–1.0; 1.0 traces everything, 0.1 traces 10%

}

// Init configures the global Sentry client. Returns true if Sentry was

// successfully enabled, false if the DSN was empty (no-op mode). Call Flush

// in a deferred shutdown handler.

//

// Critical: Sentry's Go SDK uses a global hub. Initialization must happen

// before any goroutines that might capture errors are spawned.

func Init(cfg Config) (bool, error) {

	if cfg.DSN == "" {

		log.Println("[sentry] DSN empty — telemetry disabled")

		return false, nil

	}

	err := sentry.Init(sentry.ClientOptions{

		Dsn: cfg.DSN,

		Environment: cfg.Environment,

		Release: cfg.Release,

		TracesSampleRate: cfg.TracesSampleRate,

		// AttachStacktrace adds stack traces to messages (errors get them

		// automatically). Useful for breadcrumb-driven debugging.

		AttachStacktrace: true,

		// EnableTracing must be true for transactions/spans to be sent.

		EnableTracing: cfg.TracesSampleRate > 0,

		// BeforeSend is the last hook before an event leaves the process.

		// Use it to scrub anything sensitive — Slack tokens, NIM keys, PII.

		BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {

			scrubSensitive(event)

			return event

		},
	})

	if err != nil {

		return false, fmt.Errorf("sentry init: %w", err)

	}

	log.Printf("[sentry] enabled environment=%s release=%s traces=%.2f",

		cfg.Environment, cfg.Release, cfg.TracesSampleRate)

	return true, nil

}

// Flush blocks for up to `timeout` waiting for queued events to send.

// Always call this on shutdown — Sentry's transport is async, so without

// a flush you'll lose the last few events on every restart.

func Flush(timeout time.Duration) {

	sentry.Flush(timeout)

}

// CaptureError sends an error to Sentry with optional structured context.

// The tags map becomes Sentry tags (searchable/filterable in the UI);

// extras are non-indexed metadata.

//

// Returns the original error unchanged so callers can compose it inline:

//

//	return observability.CaptureError(ctx, err, tags, nil)

func CaptureError(ctx context.Context, err error, tags map[string]string, extras map[string]interface{}) error {

	if err == nil {

		return nil

	}

	// Clone the hub so concurrent goroutines don't stomp on each other's scope.

	hub := sentry.GetHubFromContext(ctx)

	if hub == nil {

		hub = sentry.CurrentHub().Clone()

	}

	hub.WithScope(func(scope *sentry.Scope) {

		for k, v := range tags {

			scope.SetTag(k, v)

		}

		for k, v := range extras {

			scope.SetContext(k, sentry.Context{"value": v})

		}

		hub.CaptureException(err)

	})

	return err

}

// CaptureMessage sends a non-error event (e.g., a notable warning).

func CaptureMessage(ctx context.Context, msg string, level sentry.Level, tags map[string]string) {

	hub := sentry.GetHubFromContext(ctx)

	if hub == nil {

		hub = sentry.CurrentHub().Clone()

	}

	hub.WithScope(func(scope *sentry.Scope) {

		scope.SetLevel(level)

		for k, v := range tags {

			scope.SetTag(k, v)

		}

		hub.CaptureMessage(msg)

	})

}

// StartTransaction begins a top-level performance transaction.

// Always pair with `defer txn.Finish()`. The returned context carries the

// transaction so spans created from it nest correctly.

func StartTransaction(ctx context.Context, name, op string) (*sentry.Span, context.Context) {

	txn := sentry.StartTransaction(ctx, name, sentry.WithOpName(op))

	return txn, txn.Context()

}

// StartSpan creates a child span under the current transaction in ctx.

// Spans show up as nested timing bars in Sentry's performance view.

func StartSpan(ctx context.Context, op, description string) *sentry.Span {

	span := sentry.StartSpan(ctx, op)

	span.Description = description

	return span

}

// AddBreadcrumb records a navigation/event marker that's attached to any

// error captured later in this scope. Useful for "what was the bot doing

// just before this exploded" debugging.

func AddBreadcrumb(ctx context.Context, category, message string, data map[string]interface{}) {

	hub := sentry.GetHubFromContext(ctx)

	if hub == nil {

		hub = sentry.CurrentHub()

	}

	hub.AddBreadcrumb(&sentry.Breadcrumb{

		Category: category,

		Message: message,

		Data: data,

		Level: sentry.LevelInfo,

		Timestamp: time.Now(),
	}, nil)

}

// scrubSensitive removes anything that should never leave the process.

// Sentry sees stack traces and request bodies — both can carry secrets.

func scrubSensitive(event *sentry.Event) {

	// Strip Authorization headers from any captured HTTP request data.

	if event.Request != nil && event.Request.Headers != nil {

		for k := range event.Request.Headers {

			lower := toLower(k)

			if lower == "authorization" || lower == "x-slack-signature" || lower == "x-nvapi-key" {

				event.Request.Headers[k] = "[scrubbed]"

			}

		}

	}

	// Drop any contexts that look like tokens.

	for k, ctx := range event.Contexts {

		for ck, v := range ctx {

			if s, ok := v.(string); ok {

				if looksLikeSecret(s) {

					event.Contexts[k][ck] = "[scrubbed]"

				}

			}

		}

	}

}

func toLower(s string) string {

	b := make([]byte, len(s))

	for i := 0; i < len(s); i++ {

		c := s[i]

		if c >= 'A' && c <= 'Z' {

			c += 'a' - 'A'

		}

		b[i] = c

	}

	return string(b)

}

func looksLikeSecret(s string) bool {

	if len(s) < 20 {

		return false

	}

	prefixes := []string{"xoxp-", "xoxb-", "xapp-", "nvapi-", "Bearer "}

	for _, p := range prefixes {

		if len(s) >= len(p) && s[:len(p)] == p {

			return true

		}

	}

	return false

}
