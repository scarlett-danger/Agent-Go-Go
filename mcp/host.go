package mcp

import (
	"context"

	"encoding/json"

	"fmt"

	"log"

	"sync"

	"time"
)

// Host owns connections to N MCP servers and routes tool calls.

//

// Tools are identified globally by name. If two servers advertise tools

// with the same name, the first one registered wins and we log a warning —

// this is the same conflict-handling Claude Desktop uses.

type Host struct {
	mu sync.RWMutex

	clients map[string]*Client // name → client (filesystem, fetch, sqlite)

	// toolIndex maps tool name → server name so we can dispatch CallTool

	// without searching every server on each call.

	toolIndex map[string]string
}

// NewHost connects to every server in cfg in parallel. Failures are

// reported in the returned slice; the host still starts with whatever

// servers DID connect. This way one broken server doesn't kill the bot —

// you'll just see "no tools available" for the affected categories.

func NewHost(ctx context.Context, cfg map[string]ServerConfig) (*Host, []error) {

	h := &Host{

		clients: make(map[string]*Client),

		toolIndex: make(map[string]string),
	}

	type result struct {
		name string

		client *Client

		err error
	}

	resultCh := make(chan result, len(cfg))

	for name, sc := range cfg {

		go func(name string, sc ServerConfig) {

			// Each server gets 30s to do the full launch + handshake +

			// tools/list dance. Filesystem servers should take <1s; fetch

			// might take longer if it needs to install deps on first run.

			startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

			defer cancel()

			client, err := NewClient(startCtx, name, sc)

			resultCh <- result{name: name, client: client, err: err}

		}(name, sc)

	}

	var errs []error

	for i := 0; i < len(cfg); i++ {

		r := <-resultCh

		if r.err != nil {

			errs = append(errs, fmt.Errorf("connect %s: %w", r.name, r.err))

			continue

		}

		h.clients[r.name] = r.client

		// Index this server's tools.

		for _, t := range r.client.Tools() {

			if existing, dup := h.toolIndex[t.Name]; dup {

				log.Printf("[mcp] tool name conflict: %q already provided by %s, ignoring %s",

					t.Name, existing, r.name)

				continue

			}

			h.toolIndex[t.Name] = r.name

		}

		log.Printf("[mcp] connected %s (%d tools)", r.name, len(r.client.Tools()))

	}

	return h, errs

}

// AllTools returns every tool from every connected server, deduplicated by

// name (first-registered wins, matching the index built at startup).

func (h *Host) AllTools() []Tool {

	h.mu.RLock()

	defer h.mu.RUnlock()

	out := []Tool{}

	seen := map[string]bool{}

	for _, client := range h.clients {

		for _, t := range client.Tools() {

			if seen[t.Name] {

				continue

			}

			seen[t.Name] = true

			out = append(out, t)

		}

	}

	return out

}

// FilterTools returns only the tools whose names appear in `allow`. Used by

// the workflow to honor per-category tool budgets from routing.yaml.

//

// Names not present in any connected server are silently dropped — this

// way an old routing.yaml referencing a now-disconnected tool degrades

// gracefully instead of crashing the workflow.

func (h *Host) FilterTools(allow []string) []Tool {

	if len(allow) == 0 {

		return nil

	}

	allowSet := make(map[string]bool, len(allow))

	for _, n := range allow {

		allowSet[n] = true

	}

	out := []Tool{}

	for _, t := range h.AllTools() {

		if allowSet[t.Name] {

			out = append(out, t)

		}

	}

	return out

}

// CallTool routes by tool name to the right server. Returns an error if

// the tool isn't registered with any connected server.

func (h *Host) CallTool(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {

	h.mu.RLock()

	serverName, ok := h.toolIndex[name]

	if !ok {

		h.mu.RUnlock()

		return nil, fmt.Errorf("no MCP server provides tool %q", name)

	}

	client := h.clients[serverName]

	h.mu.RUnlock()

	return client.CallTool(ctx, name, args)

}

// Close terminates every server subprocess. Best-effort; logs but doesn't

// fail on individual server shutdown errors.

func (h *Host) Close() {

	h.mu.Lock()

	defer h.mu.Unlock()

	for name, c := range h.clients {

		if err := c.Close(); err != nil {

			log.Printf("[mcp] close %s: %v", name, err)

		}

	}

}
