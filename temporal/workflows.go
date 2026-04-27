package temporal

import (
	"encoding/json"

	"fmt"

	"time"

	"go.temporal.io/sdk/temporal"

	"go.temporal.io/sdk/workflow"
)

// MessageInput is the payload the Slack handler hands off to the workflow.

type MessageInput struct {
	ChannelID string

	UserID string

	Text string

	Timestamp string

	ThreadTS string

	// IsMention is true for AppMentionEvents (direct @-mentions of the bot user).
	// Mention responses are gated on the user's Away/Busy status.
	// Keyword-triggered MessageEvents ignore status and always respond.

	IsMention bool
}

// Activity name constants. Workflow references activities by string so we

// don't carry transitive dependencies (CGO from sqlite, subprocess control

// from MCP) into the workflow's compile unit.

const (
	ActivityCheckShouldRespond = "CheckShouldRespondActivity"

	ActivitySendTyping = "SendTypingActivity"

	ActivityPersistUserMsg = "PersistUserMessageActivity"

	ActivityRoute = "RouteActivity"

	ActivityLLMTurn = "LLMTurnActivity"

	ActivityExecuteTool = "ExecuteToolActivity"

	ActivityPostMessage = "PostMessageActivity"

	ActivityPersistAssistant = "PersistAssistantMessageActivity"

	ActivityMaybeSummarize = "MaybeSummarizeActivity"
)

// MaxToolIterations caps the tool-calling loop. A misbehaving model can't

// burn unbounded tokens. Anthropic's tool-calling can chain 2–6 tools for

// non-trivial questions; 8 is generous without being reckless.

const MaxToolIterations = 8

// --- Cross-activity payloads -----------------------------------------------

type RouteResult struct {
	Category string

	Provider string

	Model string

	Reason string

	Tools []string // tool names this route is allowed to invoke

}

// RouteInput carries the user message plus telemetry context. Workflow

// → Activity payloads must be plain values (no pointers, channels, etc.)

// so Temporal can serialize them.

type RouteInput struct {
	UserMessage string

	ConversationID string // becomes Langfuse session_id

	UserID string // becomes Langfuse user_id

}

// LLMTurnInput is the input to a single round-trip with the model.

// The conversation slice carries everything we want the model to see:

// prior turns from the DB plus any in-flight tool exchanges from this

// workflow's earlier loop iterations.

//

// The User/Category/Iteration fields are not used by the model — they're

// pure telemetry, forwarded into Langfuse metadata so traces are filterable

// by Slack user, routed category, and which loop iteration produced them.

type LLMTurnInput struct {
	ConversationID string

	Provider string

	Model string

	AllowedTools []string

	// IsFirstTurn signals the activity to load history from SQLite. Subsequent

	// iterations of the tool loop pass an explicit Conversation slice instead.

	IsFirstTurn bool

	// Conversation is the explicit message list for follow-up turns. Empty

	// on the first turn (the activity loads history from DB).

	Conversation []ConversationMessage

	// Langfuse trace metadata (forwarded via OpenRouter):

	UserID string // Slack user_id

	Category string // routed category, e.g. "code"

	Iteration int // tool-loop iteration counter, 0-based

}

// ConversationMessage mirrors llm.ChatMessage but defined here so the

// workflow doesn't need to import llm/. Activity translates between them.

type ConversationMessage struct {
	Role string

	Content string

	ToolCalls []ConversationToolCall

	ToolCallID string
}

type ConversationToolCall struct {
	ID string

	Name string

	Arguments json.RawMessage
}

type LLMTurnOutput struct {
	Text string

	ToolCalls []ConversationToolCall

	StopReason string

	// ConversationAfter is the full message list AFTER this turn — workflow

	// state. Letting the activity manage list construction keeps workflow

	// code free of llm.* types.

	ConversationAfter []ConversationMessage

	Provider string

	Model string
}

type ExecuteToolInput struct {
	ToolName string

	Arguments json.RawMessage
}

type ExecuteToolOutput struct {
	Result string

	IsError bool
}

// --- Workflow ---------------------------------------------------------------

func ProcessMessageWorkflow(ctx workflow.Context, input MessageInput) error {

	logger := workflow.GetLogger(ctx)

	logger.Info("workflow start",

		"channel", input.ChannelID, "user", input.UserID, "thread", input.ThreadTS)

	// Activity-option presets.

	llmOpts := workflow.ActivityOptions{

		StartToCloseTimeout: 2 * time.Minute,

		ScheduleToCloseTimeout: 5 * time.Minute,

		RetryPolicy: &temporal.RetryPolicy{

			InitialInterval: time.Second,

			BackoffCoefficient: 2.0,

			MaximumInterval: 30 * time.Second,

			MaximumAttempts: 5,

			NonRetryableErrorTypes: []string{"InvalidInputError"},
		},
	}

	toolOpts := workflow.ActivityOptions{

		// Tool calls vary wildly: filesystem reads in ms, fetch can take 30s+

		// for slow URLs. 2min is a sane outer bound.

		StartToCloseTimeout: 2 * time.Minute,

		RetryPolicy: &temporal.RetryPolicy{

			InitialInterval: time.Second,

			MaximumAttempts: 3, // tools are often non-idempotent; don't retry hard

		},
	}

	fastOpts := workflow.ActivityOptions{

		StartToCloseTimeout: 30 * time.Second,

		RetryPolicy: &temporal.RetryPolicy{

			InitialInterval: time.Second,

			BackoffCoefficient: 2.0,

			MaximumInterval: 10 * time.Second,

			MaximumAttempts: 5,
		},
	}

	llmCtx := workflow.WithActivityOptions(ctx, llmOpts)

	toolCtx := workflow.WithActivityOptions(ctx, toolOpts)

	fastCtx := workflow.WithActivityOptions(ctx, fastOpts)

	// 0. Status gate: for @mentions, only respond when Away or Busy.
	//    Keyword-triggered messages always respond regardless of status.

	if input.IsMention {

		var shouldRespond bool

		if err := workflow.ExecuteActivity(fastCtx, ActivityCheckShouldRespond).Get(ctx, &shouldRespond); err != nil {

			logger.Warn("status check failed; skipping mention response", "err", err)

			return nil

		}

		if !shouldRespond {

			logger.Info("user is active; ignoring mention")

			return nil

		}

	}

	// 1. Persist user message (idempotent).

	if err := workflow.ExecuteActivity(fastCtx, ActivityPersistUserMsg, input).Get(ctx, nil); err != nil {

		logger.Error("persist user msg failed", "err", err)

		return err

	}

	// 2. Typing indicator (best-effort).

	if err := workflow.ExecuteActivity(fastCtx, ActivitySendTyping, input.ChannelID).Get(ctx, nil); err != nil {

		logger.Warn("typing failed (continuing)", "err", err)

	}

	// 3. Route. The activity gets user/conversation context so the judge

	// LLM call appears in Langfuse with the correct session/user tags.

	routeIn := RouteInput{

		UserMessage: input.Text,

		ConversationID: conversationID(input),

		UserID: input.UserID,
	}

	var route RouteResult

	if err := workflow.ExecuteActivity(llmCtx, ActivityRoute, routeIn).Get(ctx, &route); err != nil {

		logger.Error("route failed", "err", err)

		return err

	}

	logger.Info("routed",

		"category", route.Category, "provider", route.Provider,

		"model", route.Model, "tools", len(route.Tools))

	// 5. Tool-calling loop.

	//

	// Each iteration:

	//   a. Call LLM with the current conversation

	//   b. If response has tool_calls, execute each, append results, repeat

	//   c. Otherwise we have the final text — break

	//

	// MaxToolIterations bounds the loop. An iteration counter > the cap

	// means we end the loop and use whatever text the model produced

	// alongside the tool calls (or a graceful "I tried but couldn't" if empty).

	turnInput := LLMTurnInput{

		ConversationID: conversationID(input),

		Provider: route.Provider,

		Model: route.Model,

		AllowedTools: route.Tools,

		IsFirstTurn: true,

		UserID: input.UserID,

		Category: route.Category,

		Iteration: 0,
	}

	var finalText string

	var finalProvider, finalModel string

	for iter := 0; iter < MaxToolIterations; iter++ {

		var out LLMTurnOutput

		if err := workflow.ExecuteActivity(llmCtx, ActivityLLMTurn, turnInput).Get(ctx, &out); err != nil {

			logger.Error("llm turn failed", "err", err, "iter", iter)

			return err

		}

		finalProvider = out.Provider

		finalModel = out.Model

		// No tool calls? We're done.

		if len(out.ToolCalls) == 0 {

			finalText = out.Text

			break

		}

		logger.Info("model requested tools", "iter", iter, "count", len(out.ToolCalls))

		// Execute each tool. We could parallelize via futures, but for a

		// chat bot the latency benefit isn't worth the workflow-state

		// complexity. Sequential is fine.

		convAfter := out.ConversationAfter

		for _, tc := range out.ToolCalls {

			toolIn := ExecuteToolInput{

				ToolName: tc.Name,

				Arguments: tc.Arguments,
			}

			var toolOut ExecuteToolOutput

			if err := workflow.ExecuteActivity(toolCtx, ActivityExecuteTool, toolIn).Get(ctx, &toolOut); err != nil {

				// Tool failed even after retries. Feed the error back to the

				// model as a tool result so it can recover or apologize —

				// don't fail the whole workflow.

				logger.Warn("tool failed; feeding error to model", "tool", tc.Name, "err", err)

				toolOut = ExecuteToolOutput{

					Result: fmt.Sprintf("error: %v", err),

					IsError: true,
				}

			}

			convAfter = append(convAfter, ConversationMessage{

				Role: "tool",

				Content: toolOut.Result,

				ToolCallID: tc.ID,
			})

		}

		// Set up next iteration with the augmented conversation.

		turnInput = LLMTurnInput{

			ConversationID: conversationID(input),

			Provider: route.Provider,

			Model: route.Model,

			AllowedTools: route.Tools,

			IsFirstTurn: false,

			Conversation: convAfter,

			UserID: input.UserID,

			Category: route.Category,

			Iteration: iter + 1,
		}

		// On the last allowed iteration, we'll loop one more time but the

		// LLM activity should be told to disable tools. We handle that by

		// clearing AllowedTools when we're at the cap.

		if iter == MaxToolIterations-2 {

			turnInput.AllowedTools = nil

			logger.Warn("nearing max tool iterations; disabling tools next turn")

		}

	}

	if finalText == "" {

		// Loop exhausted without a final text. Provide a soft fallback

		// rather than posting nothing.

		finalText = "Hmm, I got tangled up looking that up. Could you rephrase?"

		logger.Warn("tool loop exhausted with no final text", "max", MaxToolIterations)

	}

	// 6. Post to Slack.

	postIn := PostMessageInput{

		ChannelID: input.ChannelID,

		Text: finalText,

		ThreadTS: input.ThreadTS,
	}

	if err := workflow.ExecuteActivity(fastCtx, ActivityPostMessage, postIn).Get(ctx, nil); err != nil {

		logger.Error("post failed", "err", err)

		return err

	}

	// 8. Persist assistant reply.

	persistIn := PersistAssistantInput{

		ConversationID: conversationID(input),

		Content: finalText,

		Provider: finalProvider,

		Model: finalModel,
	}

	if err := workflow.ExecuteActivity(fastCtx, ActivityPersistAssistant, persistIn).Get(ctx, nil); err != nil {

		logger.Error("persist assistant failed (reply already sent)", "err", err)

	}

	// 9. Maybe summarize.

	if err := workflow.ExecuteActivity(llmCtx, ActivityMaybeSummarize, conversationID(input)).Get(ctx, nil); err != nil {

		logger.Warn("summarize failed (non-fatal)", "err", err)

	}

	logger.Info("workflow complete", "model_used", finalModel)

	return nil

}

func conversationID(in MessageInput) string {

	if in.ThreadTS != "" {

		return "thread:" + in.ThreadTS

	}

	return "channel:" + in.ChannelID

}

