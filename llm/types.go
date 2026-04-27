package llm

import (
	"context"

	"encoding/json"
)

// Role constants — values are wire format strings.

const (
	RoleSystem = "system"

	RoleUser = "user"

	RoleAssistant = "assistant"

	RoleTool = "tool"
)

// ChatMessage is one conversation turn. ToolCalls and ToolCallID are the

// tool-use additions:

//   - assistant message with ToolCalls = the model wants to invoke tools

//   - subsequent message with Role=tool and ToolCallID = the tool's output

type ChatMessage struct {
	Role string `json:"role"`

	Content string `json:"content"`

	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	ToolCallID string `json:"tool_call_id,omitempty"`
}

// Tool is the JSON-Schema-shaped tool descriptor we send to the model.

// Convert from MCP's mcp.Tool with FromMCP.

type Tool struct {
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall is one tool invocation requested by the model. ID is provider-

// supplied; we MUST echo it back on the corresponding tool result so the

// model can match parallel calls.

type ToolCall struct {
	ID string `json:"id"`

	Name string `json:"name"`

	Arguments json.RawMessage `json:"arguments"`
}

// CompletionRequest is what the workflow hands to a provider.

//

// Metadata is the OpenRouter passthrough field — anything you put here gets

// forwarded to your configured Langfuse integration. Common keys:

//   - session_id  — groups related calls in Langfuse (we use Slack thread_ts)

//   - user_id     — Langfuse user (we use Slack user_id)

//   - trace_name  — short label like "judge" or "expert.code"

//   - tags        — string array for filtering ("category:code", "iter:2")

type CompletionRequest struct {
	System string

	Messages []ChatMessage

	Model string

	Temperature float64

	MaxTokens int

	Tools []Tool

	Metadata map[string]interface{}
}

// CompletionResponse carries the assistant text plus optional tool calls.

type CompletionResponse struct {
	Text string

	ToolCalls []ToolCall

	StopReason string

	PromptTokens int

	CompletionTokens int

	Model string
}

// Provider is the interface used by activities. Only OpenRouter implements

// it for the MVP, but the abstraction is preserved for adding direct

// Anthropic/Moonshot later if you want them.

type Provider interface {
	Name() string

	Complete(ctx context.Context, req CompletionRequest) (*CompletionResponse, error)
}

// ToolFromMCP converts an MCP tool descriptor to an LLM tool. Both speak

// JSON Schema, so the schema is passed through unchanged.

func ToolFromMCP(name, description string, inputSchema json.RawMessage) Tool {

	if len(inputSchema) == 0 {

		inputSchema = json.RawMessage(`{"type":"object","properties":{}}`)

	}

	return Tool{Name: name, Description: description, InputSchema: inputSchema}

}
