package mcp

import (
	"io"

	"os"
)

// stderrLogger returns a writer for diagnostics from MCP subprocesses.

// Wrapped in a function so we can swap it for a structured logger later

// without touching the call sites.

func stderrLogger() io.Writer {

	return os.Stderr

}
