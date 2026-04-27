package llm

import (
	"encoding/json"
	"testing"
)

func TestToolFromMCP(t *testing.T) {
	t.Run("non-empty schema is passed through unchanged", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
		tool := ToolFromMCP("read_file", "reads a file", schema)

		if tool.Name != "read_file" {
			t.Errorf("name: got %q, want %q", tool.Name, "read_file")
		}
		if tool.Description != "reads a file" {
			t.Errorf("description: got %q, want %q", tool.Description, "reads a file")
		}
		if string(tool.InputSchema) != string(schema) {
			t.Errorf("schema: got %q, want %q", tool.InputSchema, schema)
		}
	})

	t.Run("nil schema gets a default empty-object schema", func(t *testing.T) {
		tool := ToolFromMCP("noop", "does nothing", nil)
		want := `{"type":"object","properties":{}}`
		if string(tool.InputSchema) != want {
			t.Errorf("schema: got %q, want %q", tool.InputSchema, want)
		}
	})

	t.Run("empty RawMessage gets a default empty-object schema", func(t *testing.T) {
		tool := ToolFromMCP("noop", "does nothing", json.RawMessage{})
		want := `{"type":"object","properties":{}}`
		if string(tool.InputSchema) != want {
			t.Errorf("schema: got %q, want %q", tool.InputSchema, want)
		}
	})
}
