package doctor

import (
	"path/filepath"
	"testing"

	"github.com/gregasher/bedrock-local-proxy/internal/claude"
	"github.com/gregasher/bedrock-local-proxy/internal/codex"
)

// Simultaneously running presets must not share a generated artifact. Each of
// these names is a machine-global constant or a sibling of the configuration
// file, so without the preset they would collide.
func TestGeneratedArtifactPathsAreScopedToThePreset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configPath := filepath.Join(t.TempDir(), "config.yaml")

	unnamedProfile, err := DefaultProfilePath("")
	if err != nil {
		t.Fatal(err)
	}
	presetProfile, err := DefaultProfilePath("work")
	if err != nil {
		t.Fatal(err)
	}
	if unnamedProfile != filepath.Join(home, "bedrock-local.config.toml") {
		t.Fatalf("the unnamed default must keep its current path, got %q", unnamedProfile)
	}
	if presetProfile != filepath.Join(home, "bedrock-local-work.config.toml") {
		t.Fatalf("preset profile = %q", presetProfile)
	}
	// Codex layers $CODEX_HOME/<name>.config.toml over the base config, so
	// the profile name is what the operator passes to codex --profile.
	if got := codex.ProfileName("work"); got != "bedrock-local-work" {
		t.Fatalf("profile name = %q", got)
	}
	if got := codex.ProfileName(""); got != codex.DefaultProfileName {
		t.Fatalf("unnamed profile name = %q", got)
	}

	for _, tc := range []struct {
		what   string
		get    func(string, string) (string, error)
		suffix []string
	}{
		{"codex catalog", codex.DefaultCatalogPath, []string{"codex", "models.json"}},
		{"claude settings", claude.DefaultSettingsPath, []string{"claude", "settings.json"}},
	} {
		unnamed, err := tc.get(configPath, "")
		if err != nil {
			t.Fatal(err)
		}
		scoped, err := tc.get(configPath, "work")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Dir(configPath)
		wantUnnamed := filepath.Join(append([]string{dir, "clients"}, tc.suffix...)...)
		wantScoped := filepath.Join(append([]string{dir, "clients", "work"}, tc.suffix...)...)
		if unnamed != wantUnnamed {
			t.Fatalf("%s without a preset = %q, want the unchanged %q", tc.what, unnamed, wantUnnamed)
		}
		if scoped != wantScoped {
			t.Fatalf("%s for a preset = %q, want %q", tc.what, scoped, wantScoped)
		}
	}
}
