package mcp

import (
	"bufio"
	"time"

	"context"

	"encoding/json"

	"fmt"

	"io"

	"os/exec"

	"sync"

	"sync/atomic"
)

// --- JSON-RPC envelope types ------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`

	ID int64 `json:"id"`

	Method string `json:"method"`

	Params json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`

	ID int64 `json:"id"`

	Result json.RawMessage `json:"result,omitempty"`

	Error *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code int `json:"code"`

	Message string `json:"message"`

	Data json.RawMessage `json:"data,omitempty"`
}

func (e *jsonRPCError) Error() string {

	return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message)

}

// --- MCP-specific types -----------------------------------------------------

// Tool is the shape an MCP server advertises in tools/list.

// InputSchema is a JSON Schema document — we pass it through to the LLM

// provider unchanged because both Anthropic and OpenAI-compat APIs accept

// JSON Schema directly.

type Tool struct {
	Name string `json:"name"`

	Description string `json:"description,omitempty"`

	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolResult is what a tool returns. MCP tools return content blocks (text,

// images, etc.). We flatten to a single string for now since LLM providers

// expect text in tool_result messages — image content from tools requires

// extra plumbing per provider that we don't need yet.

type ToolResult struct {
	Content []ContentBlock `json:"content"`

	IsError bool `json:"isError,omitempty"`
}

type ContentBlock struct {
	Type string `json:"type"` // "text" | "image" | "resource"

	Text string `json:"text,omitempty"` // populated when Type == "text"

}

// FlatText concatenates all text blocks. Non-text blocks are skipped with

// a placeholder so the model knows something was elided.

func (r *ToolResult) FlatText() string {

	out := ""

	for _, b := range r.Content {

		switch b.Type {

		case "text":

			out += b.Text

		default:

			out += fmt.Sprintf("[non-text content block: %s]", b.Type)

		}

	}

	return out

}

// --- Server config ----------------------------------------------------------

// ServerConfig describes how to launch a stdio MCP server. Mirrors the

// shape of Claude Desktop's claude_desktop_config.json so config files are

// portable.

type ServerConfig struct {
	Command string `yaml:"command" json:"command"`

	Args []string `yaml:"args"    json:"args,omitempty"`

	Env map[string]string `yaml:"env"     json:"env,omitempty"`
}

// --- Client (one per server) ------------------------------------------------

// Client is a long-lived connection to a single MCP server. Safe for

// concurrent calls — JSON-RPC IDs are atomic and pending requests sit in a

// map gated by mu.

type Client struct {
	name string // human-friendly server name (e.g. "filesystem")

	cmd *exec.Cmd

	stdin io.WriteCloser

	stdout *bufio.Reader

	stderr io.ReadCloser

	nextID atomic.Int64

	mu sync.Mutex

	pending map[int64]chan jsonRPCResponse

	tools []Tool // cached list from tools/list

	closeOnce sync.Once

	closed chan struct{}

	writeMu sync.Mutex // serializes writes to stdin

}

// NewClient launches the subprocess and runs the MCP initialize handshake.

// The returned client is ready for ListTools / CallTool. ctx bounds the

// startup handshake only — the subprocess outlives ctx unless Close is called.

func NewClient(ctx context.Context, name string, cfg ServerConfig) (*Client, error) {

	cmd := exec.Command(cfg.Command, cfg.Args...)

	// Inherit parent env then layer per-server overrides.

	cmd.Env = append(cmd.Environ(), envSliceFromMap(cfg.Env)...)

	stdin, err := cmd.StdinPipe()

	if err != nil {

		return nil, fmt.Errorf("stdin pipe: %w", err)

	}

	stdout, err := cmd.StdoutPipe()

	if err != nil {

		return nil, fmt.Errorf("stdout pipe: %w", err)

	}

	stderr, err := cmd.StderrPipe()

	if err != nil {

		return nil, fmt.Errorf("stderr pipe: %w", err)

	}

	if err := cmd.Start(); err != nil {

		return nil, fmt.Errorf("start mcp server %q: %w", name, err)

	}

	c := &Client{

		name: name,

		cmd: cmd,

		stdin: stdin,

		stdout: bufio.NewReader(stdout),

		stderr: stderr,

		pending: make(map[int64]chan jsonRPCResponse),

		closed: make(chan struct{}),
	}

	// Background goroutine reads responses and routes them by ID.

	go c.readLoop()

	// Drain stderr — MCP servers chat there. Keep it for debugging.

	go c.drainStderr()

	// Run the initialize handshake. If this hangs the ctx will time out

	// (caller is expected to pass a ctx with a reasonable deadline).

	if err := c.initialize(ctx); err != nil {

		_ = c.Close()

		return nil, fmt.Errorf("initialize %s: %w", name, err)

	}

	// Cache the tool list once at startup. Most servers don't add tools

	// dynamically; if yours does, refresh manually.

	tools, err := c.listTools(ctx)

	if err != nil {

		_ = c.Close()

		return nil, fmt.Errorf("tools/list %s: %w", name, err)

	}

	c.tools = tools

	return c, nil

}

func (c *Client) Name() string { return c.name }

func (c *Client) Tools() []Tool { return c.tools }

// readLoop reads newline-delimited JSON-RPC responses from stdout and hands

// them to whichever caller is waiting on that ID. MCP stdio framing is

// "one JSON object per line" — simple enough that we don't need a separate

// LSP-style Content-Length header.

func (c *Client) readLoop() {

	for {

		line, err := c.stdout.ReadBytes('\n')

		if err != nil {

			// Process exited or pipe closed — wake everyone waiting.

			c.mu.Lock()

			for id, ch := range c.pending {

				close(ch)

				delete(c.pending, id)

			}

			c.mu.Unlock()

			close(c.closed)

			return

		}

		var resp jsonRPCResponse

		if err := json.Unmarshal(line, &resp); err != nil {

			// Could be a notification (no ID) or junk. Skip.

			continue

		}

		// Notifications have ID==0 in our struct (omitted in JSON).

		// Filter by checking the raw JSON for "id".

		if resp.ID == 0 {

			continue

		}

		c.mu.Lock()

		ch, ok := c.pending[resp.ID]

		delete(c.pending, resp.ID)

		c.mu.Unlock()

		if ok {

			ch <- resp

			close(ch)

		}

	}

}

func (c *Client) drainStderr() {

	scanner := bufio.NewScanner(c.stderr)

	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {

		// MCP servers log here; surface to our own logs with a prefix so

		// you can grep "mcp/<server>:" to debug a misbehaving server.

		fmt.Fprintf(stderrLogger(), "[mcp/%s] %s\n", c.name, scanner.Text())

	}

}

// call sends a request and blocks for the matching response or ctx cancel.

func (c *Client) call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {

	id := c.nextID.Add(1)

	var paramsRaw json.RawMessage

	if params != nil {

		b, err := json.Marshal(params)

		if err != nil {

			return nil, fmt.Errorf("marshal params: %w", err)

		}

		paramsRaw = b

	}

	req := jsonRPCRequest{

		JSONRPC: "2.0",

		ID: id,

		Method: method,

		Params: paramsRaw,
	}

	reqBytes, err := json.Marshal(req)

	if err != nil {

		return nil, fmt.Errorf("marshal request: %w", err)

	}

	reqBytes = append(reqBytes, '\n')

	ch := make(chan jsonRPCResponse, 1)

	c.mu.Lock()

	c.pending[id] = ch

	c.mu.Unlock()

	c.writeMu.Lock()

	_, werr := c.stdin.Write(reqBytes)

	c.writeMu.Unlock()

	if werr != nil {

		c.mu.Lock()

		delete(c.pending, id)

		c.mu.Unlock()

		return nil, fmt.Errorf("write: %w", werr)

	}

	select {

	case resp, ok := <-ch:

		if !ok {

			return nil, fmt.Errorf("mcp connection closed before response")

		}

		if resp.Error != nil {

			return nil, resp.Error

		}

		return resp.Result, nil

	case <-ctx.Done():

		c.mu.Lock()

		delete(c.pending, id)

		c.mu.Unlock()

		return nil, ctx.Err()

	}

}

// initialize runs the MCP handshake. The client tells the server its

// protocol version and capabilities; the server replies with its own.

// We don't currently enforce capability matching — if a server doesn't

// support tools, ListTools will just return empty.

func (c *Client) initialize(ctx context.Context) error {

	params := map[string]interface{}{

		"protocolVersion": "2024-11-05",

		"capabilities": map[string]interface{}{

			// We're a tool-only client. Empty objects signal "I have this

			// capability slot but no specific options" per the MCP spec.

			"tools": map[string]interface{}{},
		},

		"clientInfo": map[string]interface{}{

			"name": "Agent-Go-Go",

			"version": "0.1.0",
		},
	}

	if _, err := c.call(ctx, "initialize", params); err != nil {

		return err

	}

	// Per spec, after a successful initialize the client MUST send an

	// "initialized" notification. Notifications have no ID — we send

	// them directly and don't expect a reply.

	notif := map[string]interface{}{

		"jsonrpc": "2.0",

		"method": "notifications/initialized",
	}

	b, _ := json.Marshal(notif)

	b = append(b, '\n')

	c.writeMu.Lock()

	_, err := c.stdin.Write(b)

	c.writeMu.Unlock()

	return err

}

func (c *Client) listTools(ctx context.Context) ([]Tool, error) {

	raw, err := c.call(ctx, "tools/list", map[string]interface{}{})

	if err != nil {

		return nil, err

	}

	var resp struct {
		Tools []Tool `json:"tools"`
	}

	if err := json.Unmarshal(raw, &resp); err != nil {

		return nil, fmt.Errorf("decode tools/list: %w", err)

	}

	return resp.Tools, nil

}

// CallTool invokes a tool by name with the given JSON arguments object.

// arguments must serialize to a JSON object matching the tool's InputSchema —

// the LLM produces this; we pass it through.

func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (*ToolResult, error) {

	if arguments == nil {

		arguments = json.RawMessage("{}")

	}

	params := map[string]interface{}{

		"name": name,

		"arguments": arguments,
	}

	raw, err := c.call(ctx, "tools/call", params)

	if err != nil {

		return nil, err

	}

	var result ToolResult

	if err := json.Unmarshal(raw, &result); err != nil {

		return nil, fmt.Errorf("decode tools/call result: %w", err)

	}

	return &result, nil

}

// Close terminates the subprocess. Idempotent.

func (c *Client) Close() error {

	var firstErr error

	c.closeOnce.Do(func() {

		_ = c.stdin.Close()

		// Give the process a moment to exit gracefully on stdin close.

		done := make(chan error, 1)

		go func() { done <- c.cmd.Wait() }()

		select {

		case err := <-done:

			firstErr = err

		case <-time.After(2 * time.Second):

			_ = c.cmd.Process.Kill()

			<-done

		}

	})

	return firstErr

}

// envSliceFromMap converts {KEY: value} to KEY=value pairs for os/exec.

func envSliceFromMap(m map[string]string) []string {

	out := make([]string, 0, len(m))

	for k, v := range m {

		out = append(out, k+"="+v)

	}

	return out

}
