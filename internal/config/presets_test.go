package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const catalogBase = `version: 1
aws:
  profile: WORK
  region: us-east-2
listen: 127.0.0.1:8787
models:
  luna:
    bedrock_model_id: A
  fable:
    bedrock_model_id: B
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPresetValidationRejects(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"unknown model", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [opus]\n", `"opus" is not a configured model`},
		{"empty model list", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8788\n    models: []\n", "must not be empty"},
		{"duplicate model", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna, luna]\n", "more than once"},
		{"missing listen", catalogBase + "presets:\n  work:\n    models: [luna]\n", "listen is required"},
		{"non-loopback listen", catalogBase + "presets:\n  work:\n    listen: 0.0.0.0:8788\n", "not loopback"},
		{"port collides with another preset", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8788\n  other:\n    listen: 127.0.0.1:8788\n", "already used by presets.other"},
		{"port collides with the top-level listen", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8787\n", "already used by the top-level listen"},
		{"uppercase name", catalogBase + "presets:\n  Work:\n    listen: 127.0.0.1:8788\n", "must start with a lowercase letter"},
		{"name with a dot", catalogBase + "presets:\n  a.b:\n    listen: 127.0.0.1:8788\n", "must start with a lowercase letter"},
		{"name starting with a hyphen", catalogBase + "presets:\n  -work:\n    listen: 127.0.0.1:8788\n", "must start with a lowercase letter"},
		{"inherited codex default outside the subset", catalogBase + "codex:\n  default_model: fable\npresets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna]\n", "effective codex.default_model"},
		{"preset session tag with a control character", catalogBase + "presets:\n  work:\n    listen: 127.0.0.1:8788\n    session_tag: \"a\\u0007b\"\n", "control characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFile(writeConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestPresetValidationAccepts(t *testing.T) {
	// A preset supplying its own client default rescues an inherited one
	// that falls outside its models.
	cfg, err := LoadFile(writeConfig(t, catalogBase+"codex:\n  default_model: fable\npresets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna]\n    codex:\n      default_model: luna\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if names := cfg.PresetNames(); len(names) != 1 || names[0] != "work" {
		t.Fatalf("preset names = %v", names)
	}
}

func TestResolveFiltersTheCatalog(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase+"presets:\n  cheap:\n    listen: 127.0.0.1:8789\n    session_tag: cheap\n    models: [luna]\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := cfg.Resolve("cheap")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.Models) != 1 {
		t.Fatalf("models = %v, want only luna", resolved.Models)
	}
	if _, blocked := resolved.Models["fable"]; blocked {
		t.Fatal("fable must not be reachable under the cheap preset")
	}
	if resolved.Listen != "127.0.0.1:8789" {
		t.Fatalf("listen = %q", resolved.Listen)
	}
	if resolved.Presets != nil {
		t.Fatal("a resolved configuration must not carry presets")
	}
	if tag := cfg.SessionTag("cheap"); tag != "cheap" {
		t.Fatalf("session tag = %q", tag)
	}
	// The catalog itself is untouched, which is what report pricing needs.
	if len(cfg.Models) != 2 {
		t.Fatalf("resolving must not narrow the source catalog: %v", cfg.Models)
	}
}

func TestResolveUnknownPresetListsTheConfiguredOnes(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase+"presets:\n  work:\n    listen: 127.0.0.1:8788\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := cfg.Resolve("missing"); err == nil || !strings.Contains(err.Error(), "configured presets: work") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveEmptyPresetIsUnchanged(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase+"presets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna]\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	resolved, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.Models) != 2 || resolved.Listen != "127.0.0.1:8787" {
		t.Fatalf("the unnamed default must be unchanged: %v %q", resolved.Models, resolved.Listen)
	}
}

func TestFreeListenAddressSkipsClaimedAndBusyPorts(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase+"presets:\n  work:\n    listen: 127.0.0.1:8788\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// 8789 is reported busy, 8788 is already claimed, so 8790 is proposed.
	got, err := cfg.FreeListenAddress(func(address string) bool { return address != "127.0.0.1:8789" })
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != "127.0.0.1:8790" {
		t.Fatalf("free address = %q, want 127.0.0.1:8790", got)
	}
}

func TestFreeListenAddressReportsAnExhaustedRange(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	_, err = cfg.FreeListenAddress(func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "8788 and 8887") {
		t.Fatalf("err = %v, want it to name the scanned range", err)
	}
}

func TestConfigWithoutPresetsIsUnaffected(t *testing.T) {
	cfg, err := LoadFile(writeConfig(t, catalogBase))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Presets) != 0 {
		t.Fatalf("presets = %v", cfg.Presets)
	}
	if _, err := cfg.Resolve("anything"); err == nil || !strings.Contains(err.Error(), "defines no presets") {
		t.Fatalf("err = %v", err)
	}
}
