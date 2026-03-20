package llm

import (
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML configuration.
type Config struct {
	Provider  string                    `yaml:"provider"`
	Providers map[string]ProviderConfig `yaml:"providers"`
}

// ProviderConfig holds connection details for a single provider.
type ProviderConfig struct {
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

// LoadConfig searches for config.yaml starting from the current directory
// and walking up to the filesystem root. Values containing ${VAR} are
// expanded from the environment.
func LoadConfig() (*Config, error) {
	path := findConfigFile("config.yaml")
	if path == "" {
		return nil, os.ErrNotExist
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// findConfigFile walks from cwd upward looking for filename.
func findConfigFile(filename string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		path := filepath.Join(dir, filename)
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
