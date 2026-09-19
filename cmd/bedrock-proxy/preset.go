package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregasher/bedrock-local-proxy/internal/config"
)

// runPresetCreate adds one preset to a configuration file. Every setting can
// be supplied by flag, so the command is fully scriptable; on a terminal the
// unanswered ones are prompted with a default in brackets.
func runPresetCreate(args []string, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	flags := flag.NewFlagSet("bedrock-proxy preset create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "path to YAML configuration")
	name := flags.String("name", "", "preset name")
	listen := flags.String("listen", "", "loopback address for this preset; defaults to the first free port")
	models := flags.String("models", "", "comma-separated models this preset allows; empty allows every model")
	sessionTag := flags.String("session-tag", "", "ingestion tag recorded in this preset's session reports")
	codexModel := flags.String("codex-model", "", "Codex default model for this preset")
	claudeModel := flags.String("claude-model", "", "Claude default model for this preset")
	dryRun := flags.Bool("dry-run", false, "print the YAML block and the insertion point without writing")
	if err := flags.Parse(args); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if flags.NArg() != 0 {
		diagnostic(stderr, "text", errors.New("preset create does not accept positional arguments"))
		return 2
	}

	path := strings.TrimSpace(*configPath)
	if path == "" {
		var err error
		if path, err = config.DefaultPath(); err != nil {
			diagnostic(stderr, "text", err)
			return 1
		}
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}

	ask := &prompter{in: bufio.NewScanner(stdin), out: stdout, enabled: interactive}
	preset := config.PresetConfig{}

	presetName, err := resolvePresetName(*name, cfg, ask)
	if err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if preset.Listen, err = resolveListen(*listen, cfg, ask); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if preset.Models, err = resolveModels(*models, cfg, ask); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if preset.SessionTag, err = resolveSessionTag(*sessionTag, presetName, ask); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	allowed := allowedSet(preset.Models, cfg)
	if preset.Codex.DefaultModel, err = resolveClientDefault("codex.default_model", *codexModel, cfg.Codex.DefaultModel, allowed, ask); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}
	if preset.Claude.DefaultModel, err = resolveClientDefault("claude.default_model", *claudeModel, cfg.Claude.DefaultModel, allowed, ask); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}

	// Validate the whole candidate configuration before touching the file,
	// so this command cannot produce a file that fails to load.
	candidate := cfg
	candidate.Presets = map[string]config.PresetConfig{}
	for existing, value := range cfg.Presets {
		candidate.Presets[existing] = value
	}
	candidate.Presets[presetName] = preset
	if err := candidate.Validate(); err != nil {
		diagnostic(stderr, "text", err)
		return 2
	}

	source, err := os.ReadFile(path)
	if err != nil {
		diagnostic(stderr, "text", fmt.Errorf("read config %q: %w", path, err))
		return 1
	}
	updated, insertErr := config.InsertPreset(source, presetName, preset)
	if errors.Is(insertErr, config.ErrPresetsNotBlockMapping) {
		fmt.Fprintf(stderr, "bedrock-proxy: %s uses a presets block this command will not edit safely.\n", path)
		fmt.Fprintln(stdout, "Add this under presets: yourself:")
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, strings.Join(config.RenderPreset(presetName, preset, 2), "\n"))
		return 1
	}
	if insertErr != nil {
		diagnostic(stderr, "text", insertErr)
		return 1
	}
	if *dryRun {
		fmt.Fprintf(stdout, "Would add preset %q to %s:\n\n", presetName, path)
		fmt.Fprintln(stdout, strings.Join(config.RenderPreset(presetName, preset, 2), "\n"))
		return 0
	}
	if err := writeConfigAtomic(path, updated); err != nil {
		diagnostic(stderr, "text", err)
		return 1
	}

	fmt.Fprintf(stdout, "Preset: %s\n", presetName)
	fmt.Fprintf(stdout, "Listening: http://%s/v1\n", preset.Listen)
	fmt.Fprintf(stdout, "Models: %s\n", strings.Join(presetModelNames(preset, cfg), ", "))
	if preset.SessionTag != "" {
		fmt.Fprintf(stdout, "Session tag: %s\n", preset.SessionTag)
	}
	fmt.Fprintf(stdout, "Config: %s\n", path)
	fmt.Fprintf(stdout, "\nNext: bedrock-proxy configure codex --preset %s\n", presetName)
	fmt.Fprintf(stdout, "      bedrock-proxy doctor --preset %s\n", presetName)
	fmt.Fprintf(stdout, "      bedrock-proxy --preset %s\n", presetName)
	return 0
}

// writeConfigAtomic replaces a configuration file through a temporary file in
// the same directory, preserving the original mode. The replacement is parsed
// and validated before the rename, so a failed insert leaves the original
// untouched.
func writeConfigAtomic(path string, contents []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect config %q: %w", path, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if _, err := config.LoadFile(temporaryPath); err != nil {
		return fmt.Errorf("refusing to write an invalid configuration: %w", err)
	}
	return os.Rename(temporaryPath, path)
}
