// Package config loads and validates the user-editable proxy configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	CurrentVersion = 1
	DefaultListen  = "127.0.0.1:8787"
)

// Config is the complete version 1 configuration file.
type Config struct {
	Version int                    `yaml:"version"`
	AWS     AWSConfig              `yaml:"aws"`
	Listen  string                 `yaml:"listen"`
	Models  map[string]ModelConfig `yaml:"models"`
}

type AWSConfig struct {
	Profile string `yaml:"profile"`
	Region  string `yaml:"region"`
}

// ModelConfig describes a public model name and its Bedrock target. Pointer
// defaults preserve the distinction between an omitted value and zero.
type ModelConfig struct {
	DisplayName      string   `yaml:"display_name,omitempty"`
	BedrockModelID   string   `yaml:"bedrock_model_id"`
	InputPerMillion  *float64 `yaml:"input_per_million,omitempty"`
	OutputPerMillion *float64 `yaml:"output_per_million,omitempty"`
	Temperature      *float64 `yaml:"temperature,omitempty"`
	MaxTokens        *int     `yaml:"max_tokens,omitempty"`
}

// DefaultPath returns the user-scoped configuration location. It does not
// depend on the process working directory.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".config", "bedrock-proxy", "config.yaml"), nil
}

// LoadFile decodes and validates a configuration file. yaml.v3's KnownFields
// mode rejects unknown fields and its decoder rejects duplicate mapping keys.
func LoadFile(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	// A second YAML document would otherwise be silently ignored.
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return Config{}, fmt.Errorf("parse config %q: multiple YAML documents are not supported", path)
	} else if !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported version %d (expected %d)", c.Version, CurrentVersion)
	}
	if strings.TrimSpace(c.AWS.Profile) == "" {
		return errors.New("aws.profile is required")
	}
	if strings.TrimSpace(c.AWS.Region) == "" {
		return errors.New("aws.region is required")
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if err := ValidateListenAddress(c.Listen); err != nil {
		return err
	}
	if len(c.Models) == 0 {
		return errors.New("models must contain at least one model")
	}
	for name, model := range c.Models {
		if strings.TrimSpace(name) == "" {
			return errors.New("model names must not be empty")
		}
		if strings.TrimSpace(model.BedrockModelID) == "" {
			return fmt.Errorf("models.%s.bedrock_model_id is required", name)
		}
		if model.InputPerMillion != nil && *model.InputPerMillion < 0 {
			return fmt.Errorf("models.%s.input_per_million must not be negative", name)
		}
		if model.OutputPerMillion != nil && *model.OutputPerMillion < 0 {
			return fmt.Errorf("models.%s.output_per_million must not be negative", name)
		}
		if model.MaxTokens != nil && *model.MaxTokens < 0 {
			return fmt.Errorf("models.%s.max_tokens must not be negative", name)
		}
	}
	return nil
}

// ValidateListenAddress accepts only loopback TCP addresses. localhost is
// accepted as the conventional loopback hostname; wildcard and other host
// addresses are rejected before binding.
func ValidateListenAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("listen must be a loopback host and port (got %q)", address)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not loopback; use %s", address, DefaultListen)
	}
	return nil
}
