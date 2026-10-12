package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/greg-asher/bedrock-local-proxy/internal/config"
)

// prompter asks for one setting at a time. When prompting is disabled it
// reports that a flag must supply the value, so the command behaves
// predictably in a script, a pipe, or CI.
type prompter struct {
	in      *bufio.Scanner
	out     io.Writer
	enabled bool
}

// ask returns the answer to one question, or fallback when the answer is
// empty. flagName names the flag that supplies this setting non-interactively.
func (p *prompter) ask(question, fallback, flagName string) (string, error) {
	if !p.enabled {
		if fallback != "" {
			return fallback, nil
		}
		return "", fmt.Errorf("--%s is required when input is not a terminal", flagName)
	}
	if fallback != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, fallback)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	if !p.in.Scan() {
		if fallback != "" {
			fmt.Fprintln(p.out)
			return fallback, nil
		}
		return "", fmt.Errorf("no answer for %q; pass --%s", question, flagName)
	}
	answer := strings.TrimSpace(p.in.Text())
	if answer == "" {
		return fallback, nil
	}
	return answer, nil
}

func (p *prompter) note(format string, args ...any) {
	if p.enabled {
		fmt.Fprintf(p.out, format, args...)
	}
}

func resolvePresetName(flagValue string, cfg config.Config, ask *prompter) (string, error) {
	name := strings.TrimSpace(flagValue)
	if name == "" {
		var err error
		if name, err = ask.ask("Preset name", "", "name"); err != nil {
			return "", err
		}
		name = strings.TrimSpace(name)
	}
	if err := config.ValidatePresetName(name); err != nil {
		return "", err
	}
	if _, exists := cfg.Presets[name]; exists {
		return "", fmt.Errorf("preset %q already exists in this configuration", name)
	}
	return name, nil
}

func resolveListen(flagValue string, cfg config.Config, ask *prompter) (string, error) {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value, nil
	}
	free, err := cfg.FreeListenAddress(nil)
	if err != nil {
		// No proposal is available, so the operator must choose one.
		return ask.ask("Listen address", "", "listen")
	}
	return ask.ask("Listen address", free, "listen")
}

func resolveModels(flagValue string, cfg config.Config, ask *prompter) ([]string, error) {
	value := strings.TrimSpace(flagValue)
	if value == "" {
		ask.note("Catalog: %s\n", strings.Join(describeCatalog(cfg), ", "))
		answer, err := ask.ask("Models this preset allows (comma-separated, empty for all)", "all", "models")
		if err != nil {
			return nil, err
		}
		value = strings.TrimSpace(answer)
		if value == "" || value == "all" {
			return nil, nil
		}
	}
	parts := strings.Split(value, ",")
	selected := make([]string, 0, len(parts))
	for _, part := range parts {
		if alias := strings.TrimSpace(part); alias != "" {
			selected = append(selected, alias)
		}
	}
	if len(selected) == 0 {
		return nil, nil
	}
	return selected, nil
}

func resolveSessionTag(flagValue, presetName string, ask *prompter) (string, error) {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value, nil
	}
	// The preset name is offered as a default only where the operator can
	// see and clear it. Off a terminal there is no prompt to show, so an
	// omitted --session-tag leaves the preset untagged rather than silently
	// tagging every one of its sessions.
	if !ask.enabled {
		return "", nil
	}
	answer, err := ask.ask("Session tag (empty for none)", presetName, "session-tag")
	if err != nil {
		return "", err
	}
	if answer = strings.TrimSpace(answer); answer == "" || answer == "none" {
		return "", nil
	}
	return answer, nil
}

// resolveClientDefault asks only when the inherited top-level default falls
// outside this preset's models, which is exactly when the configuration would
// otherwise be rejected.
func resolveClientDefault(field, flagValue, inherited string, allowed map[string]bool, ask *prompter) (string, error) {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value, nil
	}
	if inherited == "" || allowed[inherited] {
		return "", nil
	}
	ask.note("The configured %s (%q) is not in this preset's models.\n", field, inherited)
	answer, err := ask.ask(fmt.Sprintf("Preset %s", field), "", strings.ReplaceAll(strings.TrimSuffix(field, ".default_model"), ".", "")+"-model")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(answer), nil
}

// allowedSet reports which catalog aliases a preset exposes. A nil selection
// means the whole catalog.
func allowedSet(selection []string, cfg config.Config) map[string]bool {
	allowed := map[string]bool{}
	if selection == nil {
		for alias := range cfg.Models {
			allowed[alias] = true
		}
		return allowed
	}
	for _, alias := range selection {
		allowed[alias] = true
	}
	return allowed
}

func describeCatalog(cfg config.Config) []string {
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	described := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if display := strings.TrimSpace(cfg.Models[alias].DisplayName); display != "" {
			described = append(described, fmt.Sprintf("%s (%s)", alias, display))
			continue
		}
		described = append(described, alias)
	}
	return described
}

func presetModelNames(preset config.PresetConfig, cfg config.Config) []string {
	if preset.Models != nil {
		return preset.Models
	}
	aliases := make([]string, 0, len(cfg.Models))
	for alias := range cfg.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}
