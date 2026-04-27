package handlers

import (
	"context"

	"encoding/json"

	"fmt"

	"io"

	"log"

	"net/http"

	"strings"

	"github.com/getsentry/sentry-go"

	sentrygin "github.com/getsentry/sentry-go/gin"

	"github.com/gin-gonic/gin"

	"github.com/slack-go/slack"

	"github.com/slack-go/slack/slackevents"

	temporalclient "go.temporal.io/sdk/client"

	"github.com/scarlett-danger/Agent-Go-Go/observability"

	bottemporal "github.com/scarlett-danger/Agent-Go-Go/temporal"
)

// SlackHandler holds the dependencies the HTTP handler needs.

// It is constructed once at startup and reused across requests.

type SlackHandler struct {
	SigningSecret string

	BotUserID string // user ID we must NOT respond to (ourselves)

	TemporalClient temporalclient.Client

	TaskQueue string

	// TriggerKeywords, when non-empty, causes MessageEvents to be dropped
	// unless the message text contains at least one keyword (case-insensitive).
	// AppMentionEvents are never filtered.

	TriggerKeywords []string
}

func NewSlackHandler(signingSecret, botUserID string, tc temporalclient.Client, taskQueue string, keywords []string) *SlackHandler {

	return &SlackHandler{

		SigningSecret: signingSecret,

		BotUserID: botUserID,

		TemporalClient: tc,

		TaskQueue: taskQueue,

		TriggerKeywords: keywords,
	}

}

// HandleEvents is the single endpoint Slack POSTs to. It must:

//   - return 200 within ~3s or Slack will retry (potentially duplicating work)

//   - verify the request signature so untrusted callers can't trigger workflows

//   - respond to URL verification challenges synchronously

//   - dispatch real events into Temporal asynchronously

func (h *SlackHandler) HandleEvents(c *gin.Context) {

	// Pull the per-request Sentry hub installed by sentrygin. All capture

	// calls in this handler use it so they inherit the request's transaction.

	hub := sentrygin.GetHubFromContext(c)

	ctx := c.Request.Context()

	body, err := io.ReadAll(c.Request.Body)

	if err != nil {

		log.Printf("read body: %v", err)

		if hub != nil {

			hub.CaptureException(fmt.Errorf("read slack body: %w", err))

		}

		c.AbortWithStatus(http.StatusBadRequest)

		return

	}

	// --- Verify Slack signature -------------------------------------------

	// Without this, anyone who knows your URL can spam your workflow queue.

	sv, err := slack.NewSecretsVerifier(c.Request.Header, h.SigningSecret)

	if err != nil {

		log.Printf("verifier init: %v", err)

		observability.CaptureError(ctx, fmt.Errorf("verifier init: %w", err), nil, nil)

		c.AbortWithStatus(http.StatusBadRequest)

		return

	}

	if _, err := sv.Write(body); err != nil {

		log.Printf("verifier write: %v", err)

		observability.CaptureError(ctx, fmt.Errorf("verifier write: %w", err), nil, nil)

		c.AbortWithStatus(http.StatusBadRequest)

		return

	}

	if err := sv.Ensure(); err != nil {

		log.Printf("signature mismatch: %v", err)

		// Signature mismatches are common during dev (wrong secret) but also

		// indicate possible abuse in prod — capture as a warning, not an error.

		observability.CaptureMessage(ctx, "slack signature mismatch", sentry.LevelWarning, map[string]string{

			"path": c.Request.URL.Path,
		})

		c.AbortWithStatus(http.StatusUnauthorized)

		return

	}

	// --- Parse the event envelope -----------------------------------------

	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())

	if err != nil {

		log.Printf("parse event: %v", err)

		observability.CaptureError(ctx, fmt.Errorf("parse slack event: %w", err), nil, nil)

		c.AbortWithStatus(http.StatusBadRequest)

		return

	}

	// Add a breadcrumb so any later error in dispatch has context.

	observability.AddBreadcrumb(ctx, "slack.event", "received event", map[string]interface{}{

		"type": string(eventsAPIEvent.Type),
	})

	// --- URL verification (initial Slack handshake) -----------------------

	// Slack sends this once when you save the Request URL in app settings.

	if eventsAPIEvent.Type == slackevents.URLVerification {

		var r slackevents.ChallengeResponse

		if err := json.Unmarshal(body, &r); err != nil {

			observability.CaptureError(ctx, fmt.Errorf("unmarshal challenge: %w", err), nil, nil)

			c.AbortWithStatus(http.StatusBadRequest)

			return

		}

		c.Header("Content-Type", "text/plain")

		c.String(http.StatusOK, r.Challenge)

		return

	}

	// --- Real callback events: ACK first, process after -------------------

	if eventsAPIEvent.Type == slackevents.CallbackEvent {

		c.Status(http.StatusOK)

		// Detach: we MUST return to Gin in <3s. Workflow start is fast but

		// network blips happen, so we hand off to a goroutine. We pass a

		// background context (the request context dies when c returns) but

		// clone the Sentry hub so the goroutine has its own scope.

		go h.dispatch(eventsAPIEvent)

		return

	}

	c.Status(http.StatusOK)

}

// dispatch routes inner events to Temporal. Anything we don't care about is

// silently dropped — this prevents app_home_opened, reaction_added, etc. from

// triggering accidental LLM calls.

func (h *SlackHandler) dispatch(event slackevents.EventsAPIEvent) {

	switch ev := event.InnerEvent.Data.(type) {

	case *slackevents.MessageEvent:

		if ev.User == "" {

			return

		}

		if ev.Message != nil && ev.Message.Metadata.EventType == bottemporal.SlackReplyMetadataEventType {

			return

		}

		// Skip edits, deletions, channel_join noise, and other bots.

		if ev.SubType != "" || ev.BotID != "" {

			return

		}

		if !h.matchesKeywords(ev.Text) {

			return

		}

		h.startWorkflow(bottemporal.MessageInput{

			ChannelID: ev.Channel,

			UserID: ev.User,

			Text: ev.Text,

			Timestamp: ev.TimeStamp,

			ThreadTS: ev.ThreadTimeStamp,
		})

	case *slackevents.AppMentionEvent:

		// Mentions are status-gated in the workflow — only respond when Away or Busy.

		if ev.User == "" || ev.BotID != "" {

			return

		}

		h.startWorkflow(bottemporal.MessageInput{

			ChannelID: ev.Channel,

			UserID: ev.User,

			Text: ev.Text,

			Timestamp: ev.TimeStamp,

			ThreadTS: ev.ThreadTimeStamp,

			IsMention: true,
		})

	default:

		// Unhandled event types are fine — Slack sends a lot of noise.

	}

}

// matchesKeywords returns true when no keywords are configured (pass-all) or
// when the text contains at least one keyword (case-insensitive substring).

func (h *SlackHandler) matchesKeywords(text string) bool {

	if len(h.TriggerKeywords) == 0 {

		return true

	}

	lower := strings.ToLower(text)

	for _, kw := range h.TriggerKeywords {

		if strings.Contains(lower, kw) {

			return true

		}

	}

	return false

}

func (h *SlackHandler) startWorkflow(input bottemporal.MessageInput) {

	// Workflow ID is deterministic per (channel, ts), which makes Slack's

	// at-least-once retry semantics safe: a duplicate POST yields a

	// WorkflowExecutionAlreadyStarted error, not a duplicate reply.

	opts := temporalclient.StartWorkflowOptions{

		ID: fmt.Sprintf("slack-msg-%s-%s", input.ChannelID, input.Timestamp),

		TaskQueue: h.TaskQueue,
	}

	we, err := h.TemporalClient.ExecuteWorkflow(

		context.Background(),

		opts,

		bottemporal.ProcessMessageWorkflow,

		input,
	)

	if err != nil {

		log.Printf("start workflow: %v", err)

		// Capture with channel/user as tags so you can filter in Sentry.

		observability.CaptureError(context.Background(), fmt.Errorf("start workflow: %w", err),

			map[string]string{

				"channel_id": input.ChannelID,

				"workflow_id": opts.ID,
			}, nil)

		return

	}

	log.Printf("workflow started id=%s runId=%s", we.GetID(), we.GetRunID())

}
