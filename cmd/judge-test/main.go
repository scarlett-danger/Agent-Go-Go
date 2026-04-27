package main

import (
	"context"

	"fmt"

	"log"

	"os"

	"time"

	"github.com/scarlett-danger/Agent-Go-Go/config"

	"github.com/scarlett-danger/Agent-Go-Go/llm"

	"github.com/scarlett-danger/Agent-Go-Go/router"
)

// testPrompts cover each category in the default routing.yaml so we can

// see whether the judge maps prompts to the categories we'd expect a human

// to pick. These aren't strict assertions — the judge has freedom in edge

// cases — but most should land in the expected bucket.

var testPrompts = []struct {
	prompt string

	expected string
}{

	{"hi how are you", "general"},

	{"fix the null pointer bug in my Java code", "code"},

	{"what's the latest CVE for OpenSSL", "research"},

	{"if a train leaves Boston at 60mph and another leaves NYC at 80mph, when do they meet", "reasoning"},

	{"traduisez ceci en anglais: bonjour le monde", "multilingual"},

	{"write me a short poem about a tired developer", "creative"},

	{"what did we discuss yesterday about the auth refactor", "recall"},
}

func main() {

	cfg, err := config.Load()

	if err != nil {

		log.Fatalf("config load failed: %v\n\nMake sure .env has OPENROUTER_API_KEY at minimum. SLACK and OPENROUTER are required by config.Load even though we don't use Slack here — easiest fix is to set dummy values for SLACK_USER_TOKEN and SLACK_SIGNING_SECRET in .env.", err)

	}

	routingCfg, err := router.LoadConfig(cfg.RoutingConfigPath)

	if err != nil {

		log.Fatalf("routing config: %v", err)

	}

	openRouter := llm.NewOpenRouter(cfg.OpenRouterAPIKey, cfg.OpenRouterURL)

	judge := router.NewJudge(openRouter, cfg.JudgeModel, routingCfg)

	fmt.Printf("Judge model: %s\n", cfg.JudgeModel)

	fmt.Printf("Categories:  %d\n", len(routingCfg.Categories))

	fmt.Printf("Default:     %s\n\n", routingCfg.Default)

	// If the user passed a custom prompt as a CLI arg, just run that.

	if len(os.Args) > 1 {

		prompt := os.Args[1]

		runOne(judge, prompt, "")

		return

	}

	// Otherwise run the built-in test set and report pass/fail per row.

	pass, fail := 0, 0

	for _, tc := range testPrompts {

		ok := runOne(judge, tc.prompt, tc.expected)

		if ok {

			pass++

		} else {

			fail++

		}

	}

	fmt.Printf("\n=== Summary: %d/%d expected matches ===\n", pass, pass+fail)

	if fail > 0 {

		fmt.Println("Note: judge has discretion — some misses are expected on borderline prompts.")

		fmt.Println("Goal is sanity, not perfection. Worry only if everything routes to one category.")

	}

}

func runOne(judge *router.Judge, prompt, expected string) bool {

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	defer cancel()

	// Smoke-test metadata — lets you verify these calls show up in Langfuse

	// without polluting your real session data.

	metadata := map[string]interface{}{

		"session_id": "judge-smoketest",

		"user_id": "smoketest",

		"trace_name": "judge.smoketest",

		"tags": []string{"smoketest"},
	}

	start := time.Now()

	route, err := judge.Classify(ctx, prompt, metadata)

	elapsed := time.Since(start)

	if err != nil {

		fmt.Printf("  ✗  ERROR  %q\n     %v (%.1fs)\n", truncate(prompt, 60), err, elapsed.Seconds())

		return false

	}

	mark := "✓"

	matched := true

	if expected != "" && route.Category != expected {

		mark = "≈" // judge picked something different — not necessarily wrong

		matched = false

	}

	fmt.Printf("  %s  %-12s  %q\n     model=%s  reason=%q  (%.1fs)\n",

		mark, route.Category, truncate(prompt, 60),

		route.Model, route.Reason, elapsed.Seconds())

	return matched

}

func truncate(s string, n int) string {

	if len(s) <= n {

		return s

	}

	return s[:n-1] + "…"

}
