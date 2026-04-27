package mcp

import (
	"fmt"

	"os"

	"gopkg.in/yaml.v3"
)

// FileConfig matches the shape of mcp.yaml.

type FileConfig struct {
	Servers map[string]ServerConfig `yaml:"servers"`
}

// LoadConfig reads and parses an mcp.yaml file. Returns an empty (but

// non-nil) config when the file doesn't exist — that's not an error,

// it just means MCP is disabled for this run.

func LoadConfig(path string) (*FileConfig, error) {

	raw, err := os.ReadFile(path)

	if err != nil {

		if os.IsNotExist(err) {

			return &FileConfig{Servers: map[string]ServerConfig{}}, nil

		}

		return nil, fmt.Errorf("read mcp config %s: %w", path, err)

	}

	var cfg FileConfig

	if err := yaml.Unmarshal(raw, &cfg); err != nil {

		return nil, fmt.Errorf("parse mcp config: %w", err)

	}

	if cfg.Servers == nil {

		cfg.Servers = map[string]ServerConfig{}

	}

	return &cfg, nil

}
