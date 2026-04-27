package router

import (
	"fmt"

	"os"

	"strings"

	"gopkg.in/yaml.v3"
)

// Route is the resolved decision for a single message.

//

// Provider is always "openrouter" in the MVP — kept on the struct so

// telemetry tags + future direct-provider plumbing can use it without a

// breaking change.

type Route struct {
	Category string

	Provider string // always "openrouter" for now

	Model string

	Reason string

	// Tools lists MCP tool names this category may invoke. Empty (nil)

	// disables tool calling entirely for this route — faster, cheaper.

	Tools []string
}

// CategoryRule is the YAML shape for one row of routing.yaml.

type CategoryRule struct {
	Model string `yaml:"model"`

	Description string `yaml:"description"`

	Tools []string `yaml:"tools,omitempty"`
}

// Config is the full routing table.

type Config struct {
	Default string `yaml:"default"`

	Categories map[string]CategoryRule `yaml:"categories"`

	Overrides map[string]string `yaml:"overrides"`
}

// LoadConfig reads routing.yaml from disk and validates it.

func LoadConfig(path string) (*Config, error) {

	raw, err := os.ReadFile(path)

	if err != nil {

		return nil, fmt.Errorf("read routing config %s: %w", path, err)

	}

	var cfg Config

	if err := yaml.Unmarshal(raw, &cfg); err != nil {

		return nil, fmt.Errorf("parse routing config: %w", err)

	}

	if len(cfg.Categories) == 0 {

		return nil, fmt.Errorf("routing config has no categories")

	}

	if _, ok := cfg.Categories[cfg.Default]; !ok {

		return nil, fmt.Errorf("routing config default %q is not a valid category", cfg.Default)

	}

	return &cfg, nil

}

// Resolve turns a category name into a concrete Route, falling back to the

// default if the category is unknown.

func (c *Config) Resolve(category, reason string) Route {

	rule, ok := c.Categories[category]

	if !ok {

		category = c.Default

		rule = c.Categories[c.Default]

	}

	return Route{

		Category: category,

		Provider: "openrouter",

		Model: rule.Model,

		Reason: reason,

		Tools: rule.Tools,
	}

}

// CheckOverride looks for "[!name]" or "[!name] " at the start of the

// message. Returns the matched category and the message with the prefix

// stripped. Empty category means no override.

//

// Examples (with default overrides config):

//

//	"[!nemotron] write code"  →  category="code",     text="write code"

//	"[!qwen] traduire ceci"   →  category="multilingual", text="traduire ceci"

//	"regular message"         →  category="",         text="regular message"

func (c *Config) CheckOverride(text string) (category, stripped string) {

	t := strings.TrimSpace(text)

	if !strings.HasPrefix(t, "[!") {

		return "", text

	}

	end := strings.Index(t, "]")

	if end < 0 {

		return "", text

	}

	tag := strings.ToLower(strings.TrimSpace(t[2:end]))

	if tag == "" {

		return "", text

	}

	mapped, ok := c.Overrides[tag]

	if !ok {

		return "", text

	}

	rest := strings.TrimSpace(t[end+1:])

	return mapped, rest

}

// CategorySummary produces a description block to feed to the judge LLM.

// Generated dynamically from the loaded config so adding a category to

// YAML automatically updates the judge's prompt.

func (c *Config) CategorySummary() string {

	var b strings.Builder

	for name, rule := range c.Categories {

		b.WriteString("- ")

		b.WriteString(name)

		b.WriteString(": ")

		b.WriteString(rule.Description)

		b.WriteString("\n")

	}

	return b.String()

}
