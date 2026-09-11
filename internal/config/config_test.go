package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version: 1,
		AWS:     AWSConfig{Profile: "YOUR_AWS_PROFILE", Region: "us-east-2"},
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

func TestDefaultPathIsHomeScoped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".config", "bedrock-proxy", "config.yaml")
	if path != want {
		t.Fatalf("DefaultPath() = %q, want %q", path, want)
	}
}

func TestDefaultReportDirectoryUsesXDGStateOrHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	path, err := DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("default report directory = %q, want %q", path, want)
	}
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	path, err = DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("XDG report directory = %q, want %q", path, want)
	}
	t.Setenv("XDG_STATE_HOME", "relative-state")
	path, err = DefaultReportDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "bedrock-proxy", "sessions"); path != want {
		t.Fatalf("relative XDG report directory = %q, want %q", path, want)
	}
}

func TestResolveReportDirectoryPrecedenceAndRelativePaths(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveReportDirectory(configPath, "reports", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(configPath), "reports"); got != want {
		t.Fatalf("config-relative report directory = %q, want %q", got, want)
	}
	working := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(working); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	got, err = ResolveReportDirectory(configPath, "reports", "cli-reports")
	if err != nil {
		t.Fatal(err)
	}
	resolvedWorking, err := filepath.EvalSymlinks(working)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedWorking, "cli-reports"); got != want {
		t.Fatalf("CLI-relative report directory = %q, want %q", got, want)
	}
}

func TestLoadFileAcceptsOptionalReportingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := "version: 1\naws:\n  profile: p\n  region: r\nreporting:\n  directory: reports\nmodels:\n  coding:\n    bedrock_model_id: a\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reporting.Directory != "reports" {
		t.Fatalf("reporting directory = %q", cfg.Reporting.Directory)
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
		}, "finite nonnegative"},
		{"non-finite price", func(c *Config) {
			v := math.NaN()
			model := c.Models["coding"]
			model.InputPerMillion = &v
			c.Models["coding"] = model
		}, "finite nonnegative"},
		{"non-finite temperature", func(c *Config) {
			v := math.Inf(1)
			model := c.Models["coding"]
			model.Temperature = &v
			c.Models["coding"] = model
		}, "temperature must be finite"},
		{"profile whitespace", func(c *Config) { c.AWS.Profile = " profile" }, "surrounding whitespace"},
		{"region whitespace", func(c *Config) { c.AWS.Region = "us-east-2 " }, "surrounding whitespace"},
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
