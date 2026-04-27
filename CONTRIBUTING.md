# Contributing to Agent Go-Go

Thanks for your interest in contributing. This guide covers everything you need to go from zero to a merged pull request.

---

## Table of contents

- [Code of conduct](#code-of-conduct)
- [Ways to contribute](#ways-to-contribute)
- [Reporting bugs](#reporting-bugs)
- [Requesting features](#requesting-features)
- [Development setup](#development-setup)
- [Making changes](#making-changes)
- [Code style](#code-style)
- [Testing](#testing)
- [Submitting a pull request](#submitting-a-pull-request)
- [Review process](#review-process)
- [Good first issues](#good-first-issues)

---

## Code of conduct

This project follows the [Go Community Code of Conduct](https://go.dev/conduct). Be kind, be constructive, assume good intent.

---

## Ways to contribute

- Fix a bug
- Add or improve tests
- Add a new routing category to `config/routing.yaml`
- Add a new MCP tool server to `config/mcp.yaml`
- Improve documentation
- Triage issues and help reproduce bugs

---

## Reporting bugs

Search [existing issues](https://github.com/scarlett-danger/Agent-Go-Go/issues) before opening a new one.

When reporting a bug, include:

- **Go version** (`go version`)
- **OS and architecture**
- **Steps to reproduce** — be specific; include the message text and your routing config if relevant
- **Expected behaviour**
- **Actual behaviour** — paste relevant log lines, not just "it didn't work"
- **Temporal and ngrok versions** if the issue involves workflow dispatch

---

## Requesting features

Open an issue with the `enhancement` label. Describe the problem you're trying to solve, not just the solution — that makes it easier to discuss tradeoffs before anyone writes code.

For large changes (new subsystems, breaking the workflow contract, changing the memory model), open an issue first to align on design before putting in the effort of a PR.

---

## Development setup

### Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.22+ | [go.dev/dl](https://go.dev/dl/) |
| Temporal CLI | latest | `brew install temporal` or [docs](https://docs.temporal.io/cli) |
| ngrok | any | For testing Slack webhooks locally |

### Clone and configure

```bash
git clone https://github.com/scarlett-danger/Agent-Go-Go.git
cd Agent-Go-Go
cp .env.example .env
# Fill in SLACK_USER_TOKEN, SLACK_SIGNING_SECRET, OPENROUTER_API_KEY
```

### CI/CD

GitHub Actions runs automatically on every push and PR:

- **CI** (`ci.yml`) — build, vet, test, and Docker build validation. Runs on all branches.
- **Deploy** (`deploy.yml`) — deploys to Fly.io when CI passes on `main`.

To enable deployments on your fork, add one repository secret:

| Secret | Where to get it |
|---|---|
| `FLY_API_TOKEN` | [fly.io/user/personal_access_tokens](https://fly.io/user/personal_access_tokens) |

You also need a `fly.toml` committed at the repo root. Generate it once locally with `fly launch`, then commit it.

### Verify the build

```bash
go build ./...
go vet ./...
```

Both must pass cleanly before any PR.

### Run locally

```bash
# Terminal 1
temporal server start-dev

# Terminal 2
go run .

# Terminal 3
ngrok http 8080 --log=stdout
```

See the [Quick Start section of the README](README.md#quick-start-local) for the full Slack app setup.

---

## Making changes

1. **Fork** the repo and clone your fork.
2. **Create a branch** from `main`:
   ```bash
   git checkout -b your-name/short-description
   ```
3. Make your changes.
4. **Build and vet** before committing:
   ```bash
   go build ./...
   go vet ./...
   ```
5. If your change touches routing logic, run the judge smoke test:
   ```bash
   go run ./cmd/judge-test
   ```
6. Push to your fork and open a pull request against `main`.

---

## Code style

Agent Go-Go follows standard Go conventions with a few project-specific rules.

### Formatting

```bash
gofmt -w .
# or
goimports -w .
```

All submitted code must be `gofmt`-clean.

### Comments

Write comments only when the **why** is non-obvious — a hidden constraint, a subtle invariant, a workaround for a specific external behaviour. Do not describe what the code does; well-named identifiers already do that.

```go
// Good — explains a non-obvious constraint
// Workflow ID is deterministic per (channel, ts) so Slack's at-least-once
// retry semantics are safe: a duplicate POST yields WorkflowExecutionAlreadyStarted.

// Bad — restates the code
// Start the workflow with the given options.
```

### Error handling

Wrap errors with context at every layer boundary:

```go
return fmt.Errorf("persist user message: %w", err)
```

Don't wrap errors that are already wrapped by the layer below.

### No premature abstractions

Three similar lines of code is better than a premature helper. Only introduce an abstraction when you have three or more concrete uses and the abstraction genuinely simplifies all of them.

### Temporal-specific rules

- Workflow code must be **deterministic** — no `time.Now()`, `rand`, or direct I/O in workflow functions. Use `workflow.SideEffect` for non-determinism.
- Cross-workflow/activity payloads must be **plain serialisable values** — no pointers to interfaces, channels, or functions.
- Activity errors should be wrapped, not swallowed. If an activity degrades gracefully, log it and continue; don't silently eat the error.

---

## Testing

### Unit tests

```bash
go test ./...
```

The test suite is in early stages — adding tests is one of the best ways to contribute. Good targets:

- `router/` — judge prompt parsing, category resolution, override logic
- `memory/` — SQLite append, load context, summarise threshold
- `handlers/` — keyword matching, dispatch filtering

Use table-driven tests and standard `testing` + `github.com/stretchr/testify/assert`. Avoid mocking the database; use a real in-memory SQLite instance (`:memory:` path) for storage tests.

### Judge smoke test

Tests the live routing judge against the configured model. Requires `OPENROUTER_API_KEY`.

```bash
# Run the built-in prompt suite
go run ./cmd/judge-test

# Test a single prompt
go run ./cmd/judge-test "how do I profile a Go binary"

# Stub Slack vars if you don't have them set
SLACK_USER_TOKEN=xoxp-stub SLACK_SIGNING_SECRET=stub go run ./cmd/judge-test
```

If you add a new routing category to `routing.yaml`, add a representative prompt to the `testPrompts` slice in `cmd/judge-test/main.go`.

### What "passing" looks like

- `go build ./...` exits 0
- `go vet ./...` exits 0
- `go test ./...` exits 0
- Judge smoke test shows no hard errors (routing misses on borderline prompts are acceptable)

---

## Submitting a pull request

### PR checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go test ./...` passes (or existing tests are not broken)
- [ ] New behaviour has at least a basic test, or there's a clear explanation of why it can't
- [ ] If routing logic changed: judge smoke test run and results noted in the PR description
- [ ] Commits are focused — one logical change per commit

### PR description

Include:

1. **What** changed and **why**
2. **How to test it** — specific steps, not just "it works"
3. Judge smoke test output if routing is affected

Keep the title short and use the imperative mood: `Add DND status check to mention gate`, not `Added DND status checking`.

---

## Review process

- A maintainer will review within a few days.
- Expect at least one round of feedback — that's normal and not a rejection.
- Reviewers may ask you to squash commits or rebase onto `main` before merging.
- Once approved, a maintainer will merge.

---

## Good first issues

If you're new to the codebase, these are well-scoped starting points:

| Area | Task |
|---|---|
| Tests | Add unit tests for `router.CheckOverride` |
| Tests | Add unit tests for `handlers.matchesKeywords` |
| Tests | Add SQLite memory store tests using `:memory:` |
| Routing | Add a new category to `routing.yaml` with a matching smoke test prompt |
| Docs | Improve inline code comments in `temporal/workflows.go` |
| DX | Add a `Makefile` with `build`, `test`, `vet`, and `run` targets |

Look for issues labelled [`good first issue`](https://github.com/scarlett-danger/Agent-Go-Go/issues?q=label%3A%22good+first+issue%22) on GitHub for more.
