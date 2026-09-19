package config

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The fixtures below come from the design prototype. Each one checks that the
// result parses, that the new preset decodes with the values it was given,
// and that every original line survives in order.
func TestInsertPreset(t *testing.T) {
	added := PresetConfig{Listen: "127.0.0.1:8799", SessionTag: "cheap", Models: []string{"luna"}}
	for _, tc := range []struct {
		name    string
		source  string
		decline bool
		want    []string
	}{
		{
			name:   "no presets key appends a block",
			source: "version: 1\nlisten: 127.0.0.1:8787\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "presets last with comments",
			source: "version: 1\nmodels:\n  luna:\n    bedrock_model_id: X\n\n# named setups\npresets:\n  # the main work setup\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna]\n",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "presets not last inserts before the next key",
			source: "version: 1\npresets:\n  work:\n    listen: 127.0.0.1:8788\n\n# model catalog\nmodels:\n  luna:\n    bedrock_model_id: X\ncodex:\n  default_model: luna\n",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "column zero comment after the block ends it",
			source: "version: 1\npresets:\n  work:\n    listen: 127.0.0.1:8788\n# trailing note\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "blank lines inside the block do not end it",
			source: "version: 1\npresets:\n  work:\n    listen: 127.0.0.1:8788\n\n  other:\n    listen: 127.0.0.1:8790\n\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799", "other@127.0.0.1:8790", "work@127.0.0.1:8788"},
		},
		{
			name:    "flow mapping declines",
			source:  "version: 1\npresets: {work: {listen: \"127.0.0.1:8788\"}}\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			decline: true,
		},
		{
			name:   "empty presets key inserts under it",
			source: "version: 1\npresets:\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "empty presets key with a line comment",
			source: "version: 1\npresets: # add with: bedrock-proxy preset create\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "presets with only a comment under it",
			source: "version: 1\npresets:\n  # nothing yet\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "four space indentation is preserved",
			source: "version: 1\npresets:\n    work:\n        listen: 127.0.0.1:8788\nmodels:\n    luna:\n        bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "missing final newline",
			source: "version: 1\npresets:\n  work:\n    listen: 127.0.0.1:8788",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "CRLF line endings",
			source: "version: 1\r\npresets:\r\n  work:\r\n    listen: 127.0.0.1:8788\r\nmodels:\r\n  luna:\r\n    bedrock_model_id: X\r\n",
			want:   []string{"cheap@127.0.0.1:8799", "work@127.0.0.1:8788"},
		},
		{
			name:   "presets inside a comment is not the key",
			source: "version: 1\n# presets:\n#   fake:\n#     listen: 127.0.0.1:9999\nmodels:\n  luna:\n    bedrock_model_id: X\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "presets inside a block scalar is not the key",
			source: "version: 1\nmodels:\n  luna:\n    bedrock_model_id: X\n    display_name: |\n      multi line\n      presets:\n        fake: 1\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "presets at column zero inside a quoted scalar is not the key",
			source: "version: 1\nmodels:\n  luna:\n    bedrock_model_id: \"X\npresets: not-a-key\"\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
		{
			name:   "nested presets key under codex is not the top-level key",
			source: "version: 1\nmodels:\n  luna:\n    bedrock_model_id: X\ncodex:\n  default_model: luna\n",
			want:   []string{"cheap@127.0.0.1:8799"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := InsertPreset([]byte(tc.source), "cheap", added)
			if tc.decline {
				if !errors.Is(err, ErrPresetsNotBlockMapping) {
					t.Fatalf("want decline, got err=%v out=%q", err, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			got, err := decodePresets(out)
			if err != nil {
				t.Fatalf("result does not parse: %v\n%s", err, out)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("presets = %v, want %v\n%s", got, tc.want, out)
			}
			if line, ok := everyOriginalLineSurvives(tc.source, string(out)); !ok {
				t.Fatalf("lost original line %q\n%s", line, out)
			}
		})
	}
}

func TestInsertPresetIndentsAsASiblingNotAChild(t *testing.T) {
	// Taking the block indentation from the last line of the block rather
	// than from the first child nests the new preset inside the previous
	// one. That is still valid YAML, so only this assertion catches it.
	source := "version: 1\npresets:\n  work:\n    listen: 127.0.0.1:8788\n    models: [luna]\n"
	out, err := InsertPreset([]byte(source), "cheap", PresetConfig{Listen: "127.0.0.1:8799"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if !strings.Contains(string(out), "\n  cheap:\n") {
		t.Fatalf("cheap must be a sibling of work at indent 2:\n%s", out)
	}
}

func decodePresets(b []byte) ([]string, error) {
	var file struct {
		Presets map[string]PresetConfig `yaml:"presets"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(file.Presets))
	for name, preset := range file.Presets {
		out = append(out, name+"@"+preset.Listen)
	}
	sort.Strings(out)
	return out, nil
}

func everyOriginalLineSurvives(original, result string) (string, bool) {
	before := strings.Split(strings.ReplaceAll(original, "\r\n", "\n"), "\n")
	after := strings.Split(strings.ReplaceAll(result, "\r\n", "\n"), "\n")
	cursor := 0
	for _, line := range before {
		found := false
		for ; cursor < len(after); cursor++ {
			if after[cursor] == line {
				cursor++
				found = true
				break
			}
		}
		if !found {
			return line, false
		}
	}
	return "", true
}
