package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrPresetsNotBlockMapping reports a `presets:` key this package will not
// edit, such as a flow mapping written on one line. Declining is deliberate:
// guessing at unusual YAML would corrupt a file the operator maintains by
// hand, so the caller prints the block for the operator to paste instead.
var ErrPresetsNotBlockMapping = errors.New("presets is not a plain block mapping")

// RenderPreset formats one preset as YAML lines at the given indentation.
func RenderPreset(name string, preset PresetConfig, indent int) []string {
	pad := strings.Repeat(" ", indent)
	lines := []string{pad + name + ":", pad + "  listen: " + preset.Listen}
	if preset.SessionTag != "" {
		lines = append(lines, pad+"  session_tag: "+quoteTag(preset.SessionTag))
	}
	if len(preset.Models) > 0 {
		lines = append(lines, pad+"  models: ["+strings.Join(preset.Models, ", ")+"]")
	}
	if preset.Codex.DefaultModel != "" {
		lines = append(lines, pad+"  codex:", pad+"    default_model: "+preset.Codex.DefaultModel)
	}
	if preset.Claude.DefaultModel != "" || preset.Claude.SubagentModel != "" {
		lines = append(lines, pad+"  claude:")
		if preset.Claude.DefaultModel != "" {
			lines = append(lines, pad+"    default_model: "+preset.Claude.DefaultModel)
		}
		if preset.Claude.SubagentModel != "" {
			lines = append(lines, pad+"    subagent_model: "+preset.Claude.SubagentModel)
		}
	}
	return lines
}

// quoteTag quotes a session tag only when a bare scalar would not round-trip.
func quoteTag(value string) string {
	if value == strings.TrimSpace(value) && !strings.ContainsAny(value, ":#{}[],&*?|>'\"%@`") {
		return value
	}
	return strconv.Quote(value)
}

// InsertPreset adds one preset to a configuration file's text without
// re-serializing it. Everything outside the inserted lines is copied byte for
// byte, so comments, blank lines, quoting, key order, line endings, and a
// missing final newline all survive.
//
// The insertion point comes from the parsed document, not from searching the
// text. A text search cannot tell a real `presets:` key from the same
// characters inside a comment, a block scalar, or a multi-line quoted scalar,
// and it cannot determine the block's indentation correctly: taking it from
// the last line of the block nests the new preset inside the previous one,
// producing valid YAML with the wrong structure.
func InsertPreset(source []byte, name string, preset PresetConfig) ([]byte, error) {
	lines, newline, trailing := splitLines(source)
	anchor, err := presetsAnchor(source)
	if err != nil {
		return nil, err
	}
	switch {
	case !anchor.found:
		// No presets key at all: append a fresh block.
		block := append([]string{"", "presets:"}, RenderPreset(name, preset, 2)...)
		return joinLines(append(lines, block...), newline, trailing), nil
	case anchor.empty:
		// The key exists with no entries. Appending a second `presets:` key
		// would be a duplicate mapping key and the file would stop parsing,
		// so insert under the key that is already there.
		return spliceAfter(lines, anchor.keyLine, RenderPreset(name, preset, 2), newline, trailing), nil
	default:
		end := blockEnd(lines, anchor.firstChildLine)
		return spliceAfter(lines, end, RenderPreset(name, preset, anchor.indent), newline, trailing), nil
	}
}

type presetsLocation struct {
	found          bool
	empty          bool
	keyLine        int // 0-based
	firstChildLine int // 0-based
	indent         int
}

// presetsAnchor locates the top-level presets key in the parsed document.
// yaml.Node carries authoritative line and column numbers, so the anchor
// cannot be confused by text that merely looks like the key.
func presetsAnchor(source []byte) (presetsLocation, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return presetsLocation{}, fmt.Errorf("parse configuration for insertion: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) == 0 {
		return presetsLocation{}, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return presetsLocation{}, nil
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Value != "presets" {
			continue
		}
		switch {
		case value.Kind == yaml.MappingNode && value.Style == 0 && len(value.Content) > 0:
			return presetsLocation{
				found:          true,
				keyLine:        key.Line - 1,
				firstChildLine: value.Content[0].Line - 1,
				indent:         value.Content[0].Column - 1,
			}, nil
		case value.Kind == 0, value.Kind == yaml.ScalarNode && value.Tag == "!!null":
			return presetsLocation{found: true, empty: true, keyLine: key.Line - 1}, nil
		default:
			return presetsLocation{}, ErrPresetsNotBlockMapping
		}
	}
	return presetsLocation{}, nil
}

// blockEnd returns the last line belonging to a block that starts at
// firstChildLine. Blank lines do not end a block; a line at column zero does.
func blockEnd(lines []string, firstChildLine int) int {
	end := firstChildLine
	for i := firstChildLine; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			end = i
			continue
		}
		break
	}
	return end
}

func spliceAfter(lines []string, at int, block []string, newline string, trailing bool) []byte {
	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:at+1]...)
	out = append(out, block...)
	out = append(out, lines[at+1:]...)
	return joinLines(out, newline, trailing)
}

func splitLines(source []byte) ([]string, string, bool) {
	text := string(source)
	trailing := strings.HasSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\n")
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	return strings.Split(text, "\n"), newline, trailing
}

func joinLines(lines []string, newline string, trailing bool) []byte {
	cleaned := make([]string, len(lines))
	for i, line := range lines {
		cleaned[i] = strings.TrimRight(line, "\r")
	}
	text := strings.Join(cleaned, newline)
	if trailing {
		text += newline
	}
	return []byte(text)
}
