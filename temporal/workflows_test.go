package temporal

import (
	"encoding/json"
	"testing"

	"github.com/scarlett-danger/Agent-Go-Go/llm"
	"github.com/scarlett-danger/Agent-Go-Go/router"
)

func TestConversationID(t *testing.T) {
	tests := []struct {
		name  string
		input MessageInput
		want  string
	}{
		{
			name:  "threaded message uses thread_ts",
			input: MessageInput{ChannelID: "C123", ThreadTS: "1234567890.123456"},
			want:  "thread:1234567890.123456",
		},
		{
			name:  "root channel message uses channel ID",
			input: MessageInput{ChannelID: "C456", ThreadTS: ""},
			want:  "channel:C456",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := conversationID(tc.input)
			if got != tc.want {
				t.Errorf("conversationID: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLLMToWorkflowMessages(t *testing.T) {
	args := json.RawMessage(`{"path":"/tmp/test"}`)

	in := []llm.ChatMessage{
		{Role: llm.RoleUser, Content: "hello"},
		{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "tc-1", Name: "read_file", Arguments: args},
			},
		},
		{Role: llm.RoleTool, Content: "file contents here", ToolCallID: "tc-1"},
	}

	out := llmToWorkflowMessages(in)

	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if out[0].Role != llm.RoleUser || out[0].Content != "hello" {
		t.Errorf("msg[0]: got role=%q content=%q", out[0].Role, out[0].Content)
	}
	if len(out[1].ToolCalls) != 1 {
		t.Fatalf("msg[1] should have 1 tool call, got %d", len(out[1].ToolCalls))
	}
	if out[1].ToolCalls[0].Name != "read_file" || out[1].ToolCalls[0].ID != "tc-1" {
		t.Errorf("tool call: got %+v", out[1].ToolCalls[0])
	}
	if out[2].ToolCallID != "tc-1" || out[2].Content != "file contents here" {
		t.Errorf("msg[2] tool result: got %+v", out[2])
	}
}

func TestWorkflowToLLMMessages(t *testing.T) {
	args := json.RawMessage(`{"path":"/tmp/test"}`)

	in := []ConversationMessage{
		{Role: llm.RoleUser, Content: "write some code"},
		{
			Role: llm.RoleAssistant,
			ToolCalls: []ConversationToolCall{
				{ID: "tc-2", Name: "write_file", Arguments: args},
			},
		},
		{Role: llm.RoleTool, Content: "wrote 42 bytes", ToolCallID: "tc-2"},
	}

	out := workflowToLLMMessages(in)

	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if out[0].Role != llm.RoleUser || out[0].Content != "write some code" {
		t.Errorf("msg[0]: got role=%q content=%q", out[0].Role, out[0].Content)
	}
	if len(out[1].ToolCalls) != 1 {
		t.Fatalf("msg[1] should have 1 tool call, got %d", len(out[1].ToolCalls))
	}
	if out[1].ToolCalls[0].Name != "write_file" {
		t.Errorf("tool call name: got %q, want %q", out[1].ToolCalls[0].Name, "write_file")
	}
	if out[2].ToolCallID != "tc-2" {
		t.Errorf("msg[2] ToolCallID: got %q, want %q", out[2].ToolCallID, "tc-2")
	}
}

func TestStripOverrideOnLastUser(t *testing.T) {
	cfg := &router.Config{
		Default: "general",
		Categories: map[string]router.CategoryRule{
			"general": {Model: "m", Description: "d"},
			"code":    {Model: "m", Description: "d"},
		},
		Overrides: map[string]string{"code": "code"},
	}

	t.Run("strips override prefix from last user message", func(t *testing.T) {
		history := []llm.ChatMessage{
			{Role: llm.RoleAssistant, Content: "previous reply"},
			{Role: llm.RoleUser, Content: "[!code] write a test"},
		}
		out := stripOverrideOnLastUser(history, cfg)

		if out[1].Content != "write a test" {
			t.Errorf("expected stripped text, got %q", out[1].Content)
		}
		// Original should be unchanged (copy semantics).
		if history[1].Content != "[!code] write a test" {
			t.Error("original history slice should not be mutated")
		}
	})

	t.Run("no override leaves messages unchanged", func(t *testing.T) {
		history := []llm.ChatMessage{
			{Role: llm.RoleUser, Content: "just a normal message"},
		}
		out := stripOverrideOnLastUser(history, cfg)
		if out[0].Content != "just a normal message" {
			t.Errorf("expected unchanged content, got %q", out[0].Content)
		}
	})

	t.Run("empty history returns empty", func(t *testing.T) {
		out := stripOverrideOnLastUser(nil, cfg)
		if out != nil {
			t.Errorf("expected nil for nil input, got %v", out)
		}
	})
}
