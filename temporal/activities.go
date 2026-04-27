package temporal

import (
	"context"

	"fmt"

	"log"

	"strings"

	"github.com/slack-go/slack"

	"github.com/scarlett-danger/Agent-Go-Go/llm"

	"github.com/scarlett-danger/Agent-Go-Go/mcp"

	"github.com/scarlett-danger/Agent-Go-Go/memory"

	"github.com/scarlett-danger/Agent-Go-Go/observability"

	"github.com/scarlett-danger/Agent-Go-Go/router"
)

// Activities holds every long-lived dependency. Registering this struct on

// the Temporal worker exposes each exported method as an activity keyed by

// method name (matching the Activity* constants in workflows.go).

//

// MVP simplification: a single OpenRouter client serves every model — no

// registry needed. The model is selected per-request via the Model field

// in CompletionRequest.

type Activities struct {
	SlackClient *slack.Client

	BotUserID string // Slack user ID of the account running the bot

	Store *memory.Store

	OpenRouter *llm.OpenRouter

	Router *router.Config

	Judge *router.Judge

	MCPHost *mcp.Host

	SystemPrompt string

	MaxTurns int

	SummarizeAt int

	SummarizeFold int

	SummarizerModel string // typically equal to JudgeModel

}

// --- Slack/persistence activities ------------------------------------------

type PostMessageInput struct {
	ChannelID string

	Text string

	ThreadTS string
}

// SlackReplyMetadataEventType marks assistant-authored messages so inbound
// event handlers can ignore them without user-ID-based loop guards.
const SlackReplyMetadataEventType = "agent_gogo.reply"

type PersistAssistantInput struct {
	ConversationID string

	Content string

	Provider string

	Model string
}

func (a *Activities) SendTypingActivity(ctx context.Context, channelID string) error {

	// Slack's HTTP Web API does NOT support typing indicators — those are a

	// Socket Mode signal. To enable real typing dots, run a Socket Mode

	// client alongside the HTTP server (with an xapp- token) and call its

	// SendMessage with Type: "typing" from this activity.

	log.Printf("[typing] would emit typing on channel=%s (Socket Mode required for real)", channelID)

	return nil

}

// CheckShouldRespondActivity returns true when the bot's own Slack status
// indicates the user is unavailable (Away presence or Busy/DND snooze active).
// If either Slack API call fails we fail safe — return false so the bot stays
// silent rather than spamming when the user is actually at their desk.

func (a *Activities) CheckShouldRespondActivity(ctx context.Context) (bool, error) {

	presence, err := a.SlackClient.GetUserPresenceContext(ctx, a.BotUserID)

	if err != nil {

		log.Printf("[status] presence check failed: %v", err)

		observability.CaptureError(ctx, fmt.Errorf("get presence: %w", err),
			map[string]string{"activity": "CheckShouldRespondActivity"}, nil)

		return false, fmt.Errorf("get presence: %w", err)

	}

	if presence.Presence == "away" {

		log.Printf("[status] user is away — will respond")

		return true, nil

	}

	// Presence is active; check whether the user has manually set Busy (DND snooze).

	dnd, err := a.SlackClient.GetDNDInfoContext(ctx, &a.BotUserID)

	if err != nil {

		// DND check is best-effort — don't block if the endpoint is flaky.

		log.Printf("[status] dnd check failed (fail-safe: no response): %v", err)

		return false, nil

	}

	if dnd.SnoozeEnabled {

		log.Printf("[status] user is busy (DND snooze active) — will respond")

		return true, nil

	}

	log.Printf("[status] user is active — skipping auto-response")

	return false, nil

}

func (a *Activities) PersistUserMessageActivity(ctx context.Context, input MessageInput) error {

	convID := memory.ConversationIDFor(input.ChannelID, input.ThreadTS)

	created, err := a.Store.AppendUserMessage(ctx, convID, input.Text, input.Timestamp)

	if err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "PersistUserMessageActivity"}, nil)

		return fmt.Errorf("persist user: %w", err)

	}

	if !created {

		log.Printf("[persist] duplicate slack_ts=%s on conv=%s — ignored", input.Timestamp, convID)

	}

	return nil

}

func (a *Activities) PostMessageActivity(ctx context.Context, in PostMessageInput) error {

	opts := []slack.MsgOption{

		slack.MsgOptionText(in.Text, false),

		slack.MsgOptionAsUser(true),

		slack.MsgOptionMetadata(slack.SlackMetadata{
			EventType: SlackReplyMetadataEventType,
			EventPayload: map[string]any{
				"source": "agent-go-go",
			},
		}),

		slack.MsgOptionDisableLinkUnfurl(),
	}

	if in.ThreadTS != "" {

		opts = append(opts, slack.MsgOptionTS(in.ThreadTS))

	}

	if _, _, err := a.SlackClient.PostMessageContext(ctx, in.ChannelID, opts...); err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "PostMessageActivity", "channel_id": in.ChannelID}, nil)

		return fmt.Errorf("slack post: %w", err)

	}

	return nil

}

func (a *Activities) PersistAssistantMessageActivity(ctx context.Context, in PersistAssistantInput) error {

	if err := a.Store.AppendAssistantMessage(ctx, in.ConversationID, in.Content, in.Provider, in.Model); err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "PersistAssistantMessageActivity"}, nil)

		return fmt.Errorf("persist assistant: %w", err)

	}

	return nil

}

// --- Routing ----------------------------------------------------------------

func (a *Activities) RouteActivity(ctx context.Context, in RouteInput) (RouteResult, error) {

	// User override prefix → skip the judge entirely.

	if cat, _ := a.Router.CheckOverride(in.UserMessage); cat != "" {

		r := a.Router.Resolve(cat, "user override prefix")

		return RouteResult{

			Category: r.Category, Provider: r.Provider, Model: r.Model,

			Reason: r.Reason, Tools: r.Tools,
		}, nil

	}

	// Otherwise, ask the judge. Pass session/user metadata so the judge call

	// is grouped with the expert call in Langfuse.

	md := map[string]interface{}{

		"session_id": in.ConversationID,

		"user_id": in.UserID,

		"tags": []string{"phase:judge"},
	}

	r, err := a.Judge.Classify(ctx, in.UserMessage, md)

	if err != nil {

		// Classify already returned a default route; capture for Sentry.

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "RouteActivity", "fallback": "default"}, nil)

	}

	return RouteResult{

		Category: r.Category, Provider: r.Provider, Model: r.Model,

		Reason: r.Reason, Tools: r.Tools,
	}, nil

}

// --- LLM turn (one round-trip with the model) -------------------------------

// LLMTurnActivity does a single inference round. First turn loads history

// from SQLite; follow-up turns use the explicit Conversation passed in.

//

// Builds Langfuse metadata from the workflow context — every call is

// tagged with session, user, category, and iteration so traces are

// filterable in the Langfuse UI.

func (a *Activities) LLMTurnActivity(ctx context.Context, in LLMTurnInput) (LLMTurnOutput, error) {

	// Build messages.

	var messages []llm.ChatMessage

	if in.IsFirstTurn {

		history, err := a.Store.LoadContext(ctx, in.ConversationID, a.MaxTurns)

		if err != nil {

			observability.CaptureError(ctx, err,

				map[string]string{"activity": "LLMTurnActivity", "conv_id": in.ConversationID}, nil)

			return LLMTurnOutput{}, fmt.Errorf("load context: %w", err)

		}

		messages = stripOverrideOnLastUser(history, a.Router)

	} else {

		messages = workflowToLLMMessages(in.Conversation)

	}

	// Resolve allowed MCP tools → llm.Tool list.

	var tools []llm.Tool

	if len(in.AllowedTools) > 0 && a.MCPHost != nil {

		mcpTools := a.MCPHost.FilterTools(in.AllowedTools)

		tools = make([]llm.Tool, len(mcpTools))

		for i, mt := range mcpTools {

			tools[i] = llm.ToolFromMCP(mt.Name, mt.Description, mt.InputSchema)

		}

	}

	// Build Langfuse metadata. OpenRouter forwards this transparently.

	traceName := "expert"

	if in.Category != "" {

		traceName = "expert." + in.Category

	}

	metadata := map[string]interface{}{

		"session_id": in.ConversationID,

		"user_id": in.UserID,

		"trace_name": traceName,

		"tags": []string{

			"phase:expert",

			"category:" + in.Category,

			fmt.Sprintf("iter:%d", in.Iteration),
		},
	}

	resp, err := a.OpenRouter.Complete(ctx, llm.CompletionRequest{

		System: a.SystemPrompt,

		Messages: messages,

		Model: in.Model,

		Tools: tools,

		Metadata: metadata,
	})

	if err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{

				"activity": "LLMTurnActivity",

				"model": in.Model,

				"category": in.Category,
			}, nil)

		return LLMTurnOutput{}, fmt.Errorf("openrouter complete: %w", err)

	}

	// Build the conversation-after slice for the workflow loop.

	convAfter := llmToWorkflowMessages(messages)

	assistantMsg := ConversationMessage{Role: "assistant", Content: resp.Text}

	for _, tc := range resp.ToolCalls {

		assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, ConversationToolCall{

			ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})

	}

	convAfter = append(convAfter, assistantMsg)

	out := LLMTurnOutput{

		Text: resp.Text,

		StopReason: resp.StopReason,

		ConversationAfter: convAfter,

		Provider: in.Provider,

		Model: resp.Model,
	}

	for _, tc := range resp.ToolCalls {

		out.ToolCalls = append(out.ToolCalls, ConversationToolCall{

			ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
		})

	}

	return out, nil

}

// --- Tool execution ---------------------------------------------------------

func (a *Activities) ExecuteToolActivity(ctx context.Context, in ExecuteToolInput) (ExecuteToolOutput, error) {

	if a.MCPHost == nil {

		return ExecuteToolOutput{}, fmt.Errorf("mcp host not configured")

	}

	result, err := a.MCPHost.CallTool(ctx, in.ToolName, in.Arguments)

	if err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "ExecuteToolActivity", "tool": in.ToolName}, nil)

		return ExecuteToolOutput{}, fmt.Errorf("call tool %s: %w", in.ToolName, err)

	}

	return ExecuteToolOutput{

		Result: result.FlatText(),

		IsError: result.IsError,
	}, nil

}

// --- Summarization ----------------------------------------------------------

// MaybeSummarizeActivity folds the oldest unsummarized turns into a rolling

// summary if the conversation has grown beyond the configured threshold.

// Uses the same small/fast judge model — summaries don't need expert quality.

func (a *Activities) MaybeSummarizeActivity(ctx context.Context, convID string) error {

	count, err := a.Store.CountMessages(ctx, convID)

	if err != nil {

		return fmt.Errorf("count: %w", err)

	}

	if count < a.SummarizeAt {

		return nil

	}

	oldest, lastID, err := a.Store.OldestUnsummarized(ctx, convID, a.SummarizeFold)

	if err != nil {

		return fmt.Errorf("load oldest: %w", err)

	}

	if len(oldest) == 0 {

		return nil

	}

	var b strings.Builder

	for _, m := range oldest {

		b.WriteString(m.Role)

		b.WriteString(": ")

		b.WriteString(m.Content)

		b.WriteString("\n")

	}

	resp, err := a.OpenRouter.Complete(ctx, llm.CompletionRequest{

		System: `You are a conversation summarizer. Compress the following exchange into a brief

factual summary (3–5 sentences max). Preserve names, numbers, decisions,

and unresolved questions. Do not editorialize or add detail not in the text.`,

		Messages: []llm.ChatMessage{

			{Role: llm.RoleUser, Content: b.String()},
		},

		Model: a.SummarizerModel,

		Temperature: 0.2,

		MaxTokens: 400,

		Metadata: map[string]interface{}{

			"session_id": convID,

			"trace_name": "summarizer",

			"tags": []string{"phase:summarizer"},
		},
	})

	if err != nil {

		observability.CaptureError(ctx, err,

			map[string]string{"activity": "MaybeSummarizeActivity"}, nil)

		return fmt.Errorf("summarize: %w", err)

	}

	if err := a.Store.UpsertSummary(ctx, convID, resp.Text, lastID); err != nil {

		return fmt.Errorf("upsert summary: %w", err)

	}

	return nil

}

// --- Helpers ---------------------------------------------------------------

func stripOverrideOnLastUser(history []llm.ChatMessage, cfg *router.Config) []llm.ChatMessage {

	if len(history) == 0 {

		return history

	}

	out := make([]llm.ChatMessage, len(history))

	copy(out, history)

	for i := len(out) - 1; i >= 0; i-- {

		if out[i].Role == llm.RoleUser {

			if _, stripped := cfg.CheckOverride(out[i].Content); stripped != out[i].Content {

				out[i] = llm.ChatMessage{Role: out[i].Role, Content: stripped}

			}

			break

		}

	}

	return out

}

func llmToWorkflowMessages(in []llm.ChatMessage) []ConversationMessage {

	out := make([]ConversationMessage, len(in))

	for i, m := range in {

		out[i] = ConversationMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}

		for _, tc := range m.ToolCalls {

			out[i].ToolCalls = append(out[i].ToolCalls, ConversationToolCall{

				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})

		}

	}

	return out

}

func workflowToLLMMessages(in []ConversationMessage) []llm.ChatMessage {

	out := make([]llm.ChatMessage, len(in))

	for i, m := range in {

		out[i] = llm.ChatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}

		for _, tc := range m.ToolCalls {

			out[i].ToolCalls = append(out[i].ToolCalls, llm.ToolCall{

				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})

		}

	}

	return out

}
