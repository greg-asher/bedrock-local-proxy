package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version: 1,
		AWS:     AWSConfig{Profile: "Halo-Win-Agent-Execution", Region: "us-east-2"},
		Models: map[string]ModelConfig{
			"coding": {BedrockModelID: "model-a"},
			"fast":   {BedrockModelID: "model-a"},
		},
	}
}

func TestValidateDefaultsListenAndAllowsSharedTarget(t *testing.T) {
	cfg := validConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("listen = %q, want %q", cfg.Listen, DefaultListen)
	}
}

func TestValidateRejectsNonLoopbackAndInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"wildcard", func(c *Config) { c.Listen = "0.0.0.0:8787" }, "not loopback"},
		{"missing profile", func(c *Config) { c.AWS.Profile = "" }, "aws.profile is required"},
		{"negative price", func(c *Config) {
			v := -1.0
			model := c.Models["coding"]
			model.InputPerMillion = &v
			c.Models["coding"] = model
		}, "must not be negative"},
		{"missing target", func(c *Config) { c.Models["coding"] = ModelConfig{} }, "bedrock_model_id is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.edit(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestLoadFileRejectsUnknownAndDuplicateFields(t *testing.T) {
	tests := []string{
		"version: 1\naws:\n  profile: p\n  region: r\nmodels:\n  coding:\n    bedrock_model_id: a\n    extra: x\n",
		"version: 1\nversion: 1\naws:\n  profile: p\n  region: r\nmodels:\n  coding:\n    bedrock_model_id: a\n",
	}
	for _, contents := range tests {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadFile(path); err == nil {
			t.Fatalf("LoadFile(%q) unexpectedly succeeded", contents)
		}
	}
}
