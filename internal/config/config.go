// Package config loads and validates the user-editable proxy configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"math"
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
	Version   int                    `yaml:"version"`
	AWS       AWSConfig              `yaml:"aws"`
	Listen    string                 `yaml:"listen"`
	Reporting ReportingConfig        `yaml:"reporting,omitempty"`
	Models    map[string]ModelConfig `yaml:"models"`
}

type AWSConfig struct {
	Profile string `yaml:"profile"`
	Region  string `yaml:"region"`
}

// ReportingConfig controls the parent directory for durable per-session
// reports. An empty directory uses the user-scoped default.
type ReportingConfig struct {
	Directory string `yaml:"directory,omitempty"`
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

// DefaultReportDirectory returns the user-scoped parent directory for session
// reports. XDG_STATE_HOME must be absolute when supplied; a relative value is
// ignored so reports never depend on an incidental working directory.
func DefaultReportDirectory() (string, error) {
	if stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); stateHome != "" && filepath.IsAbs(stateHome) {
		return filepath.Join(stateHome, "bedrock-proxy", "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"), nil
}

// ResolveReportDirectory chooses the report parent from a CLI override,
// configuration, or the default. Relative CLI paths use the working
// directory; relative configuration paths use the configuration file's
// directory.
func ResolveReportDirectory(configPath, configuredDirectory, override string) (string, error) {
	if value := strings.TrimSpace(override); value != "" {
		path, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("resolve report directory %q: %w", value, err)
		}
		return filepath.Clean(path), nil
	}
	if value := strings.TrimSpace(configuredDirectory); value != "" {
		if filepath.IsAbs(value) {
			return filepath.Clean(value), nil
		}
		absoluteConfig, err := filepath.Abs(configPath)
		if err != nil {
			return "", fmt.Errorf("resolve config path %q: %w", configPath, err)
		}
		return filepath.Clean(filepath.Join(filepath.Dir(absoluteConfig), value)), nil
	}
	return DefaultReportDirectory()
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
	if c.AWS.Profile != strings.TrimSpace(c.AWS.Profile) {
		return errors.New("aws.profile must not have surrounding whitespace")
	}
	if strings.TrimSpace(c.AWS.Region) == "" {
		return errors.New("aws.region is required")
	}
	if c.AWS.Region != strings.TrimSpace(c.AWS.Region) {
		return errors.New("aws.region must not have surrounding whitespace")
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
		if model.InputPerMillion != nil && (!finite(*model.InputPerMillion) || *model.InputPerMillion < 0) {
			return fmt.Errorf("models.%s.input_per_million must be a finite nonnegative number", name)
		}
		if model.OutputPerMillion != nil && (!finite(*model.OutputPerMillion) || *model.OutputPerMillion < 0) {
			return fmt.Errorf("models.%s.output_per_million must be a finite nonnegative number", name)
		}
		if model.Temperature != nil && !finite(*model.Temperature) {
			return fmt.Errorf("models.%s.temperature must be finite", name)
		}
		if model.MaxTokens != nil && *model.MaxTokens < 0 {
			return fmt.Errorf("models.%s.max_tokens must not be negative", name)
		}
	}
	return nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
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
