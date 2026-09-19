# Named Presets Change Design

## Executive Summary

Add a **preset**: a named entry in `config.yaml` that sets a port, a session tag, and which of the configured models it exposes. `bedrock-proxy --preset work` runs the proxy under that preset; every subcommand accepts the same flag.

The seam is a single resolution step in `internal/config`. A preset resolves to an ordinary `config.Config` whose `Models` map has been filtered to the preset's allowed aliases and whose `Listen` has been replaced. Nothing downstream learns that presets exist — the server, accounting, catalog generation, and diagnostics keep consuming the same `Config` they consume today. Model restriction then enforces itself everywhere at once, because every consumer already keys off `cfg.Models`.

Two areas need real work beyond that: the generated client artifacts, whose names are machine-global constants today and would collide between simultaneously running presets; and report pricing, which must keep seeing the unfiltered model set.

A new `bedrock-proxy preset create` scans for a free port, asks for the remaining settings on a terminal, takes flags otherwise, and inserts the preset into `config.yaml` as text so the operator's comments survive. It is the first interactive command in this binary and the first that writes a configuration file.

## Requested Outcome

An operator packages a setup under a short name and launches it directly. A preset sets the session tag, the port, and which models are allowed. All presets share one model catalog; a preset narrows it rather than redefining it.

Scenarios that change:

1. `bedrock-proxy --preset work` binds that preset's port, tags its session report, and serves only its models. Startup names the preset.
2. A second preset starts in another terminal. Both run. Neither overwrites the other's generated client files.
3. A client asks a preset for a model that preset does not allow. The request is refused.
4. `bedrock-proxy configure codex --preset work` produces Codex metadata for that preset without disturbing an existing setup.
5. `bedrock-proxy report --preset work` reports usage for that preset only.
6. `bedrock-proxy preset create` proposes a free port, asks for the name, allowed models, and session tag, and adds the preset to `config.yaml` without disturbing the rest of the file.

Constraints:

- An operator with no presets keeps today's behavior exactly, including today's file paths and today's `bedrock-local` Codex name.
- A preset name must be safe as a filename segment and as a Codex profile name.
- Model definitions, including pricing and capabilities, are written once.
- A command that writes `config.yaml` leaves it loadable, or leaves it untouched.

## Relevant Current Behavior

`run` dispatches on the first positional argument ([main.go:64](../../cmd/bedrock-proxy/main.go:64)). `report`, `configure codex`, `configure claude`, and `doctor` each build their own flag set; anything else falls through to `runProxy` with the whole argument vector. A subcommand keyword must come first, and a flag placed before it is parsed as a proxy flag.

All five subcommands accept `--config`, falling back to the single machine-wide `$HOME/.config/bedrock-proxy/config.yaml` ([config.go:74](../../internal/config/config.go:74)). There is no way to select among several setups.

**`cfg.Models` is already the single gate for model access.** Every request path looks the alias up in that map — [chat.go:209](../../internal/server/chat.go:209), [messages.go:141](../../internal/server/messages.go:141), [responses.go:146](../../internal/server/responses.go:146) — and an unknown alias already produces a proper `model_not_found` error ([server.go:226](../../internal/server/server.go:226)). Catalog generation, the Claude picker, the `/v1/models` listing, and accounting pricing all read the same map. This is why filtering it is sufficient enforcement.

**`Validate()` already cross-checks named aliases** against `c.Models`: `codex.default_model` at [config.go:176](../../internal/config/config.go:176) and the two Claude selections at [config.go:189](../../internal/config/config.go:189). A preset's allowed-model list validates by the same pattern in the same pass.

**The session tag exists only as a CLI flag** ([main.go:257](../../cmd/bedrock-proxy/main.go:257)) and is never read from configuration.

Two kinds of derived name exist today.

*Sibling of the configuration file.* `codex.DefaultCatalogPath` is `<dir of config>/clients/codex/models.json` ([catalog.go:310](../../internal/codex/catalog.go:310)) and `claude.DefaultSettingsPath` is `clients/claude/settings.json` ([settings.go:255](../../internal/claude/settings.go:255)). Both overwrite atomically. With presets sharing one `config.yaml`, these are now a *guaranteed* collision rather than a conditional one — two presets always produce the identical path.

*Package constant or fixed path.* `codex.DefaultProfileName` is the literal `bedrock-local` ([catalog.go:25](../../internal/codex/catalog.go:25)), and `doctor.DefaultProfilePath` ignores the configuration path entirely, returning one machine-wide `$CODEX_HOME/bedrock-local.config.toml` ([doctor.go:255](../../internal/doctor/doctor.go:255)). `configure codex` tells the operator to save there ([main.go:203](../../cmd/bedrock-proxy/main.go:203)). A second setup overwrites the first, and `doctor` then reads the survivor plus the catalog path recorded inside it ([doctor.go:125](../../internal/doctor/doctor.go:125)), so it can silently validate the wrong setup.

`config.DefaultListen` is `127.0.0.1:8787` ([config.go:20](../../internal/config/config.go:20)).

Session directory naming does **not** collide: nanosecond timestamp, process ID, and a random suffix, created with `os.Mkdir` ([session.go:150](../../internal/accounting/session.go:150)). Concurrent proxies are already safe there. Only the shared parent tree mixes usage.

## Affected Surface

| Area | Change |
| --- | --- |
| `internal/config` | `presets:` schema, name and cross-reference validation, port-uniqueness validation, `Resolve(preset)`, the free-port scan, and the node-anchored preset insert |
| `cmd/bedrock-proxy` | `--preset` on all five subcommands; the new `preset create` subcommand; a stdin reader and an interactivity flag on `run`; preset in startup output; pass the unfiltered model set to `report` |
| `internal/codex` | `DefaultCatalogPath` and the generated profile name take the preset |
| `internal/claude` | `DefaultSettingsPath` takes the preset |
| `internal/doctor` | `DefaultProfilePath` takes the preset |
| `internal/accounting` | session manifest and events record the preset name |
| `internal/reports` | preset breakdown and `--preset` filter |
| Docs | `README.md`, `config.example.yaml` |

`internal/server`, `internal/transport`, and `internal/pricing` do not change at all. They receive a `Config` and behave as they always have; the filtering happened before they saw it.

`install.sh` does not change. It scaffolds `config.example.yaml`, which gains a commented `presets:` example.

## External Findings That Shaped the Design

**Codex resolves `--profile NAME` by overlaying a separate file, not a `[profiles.NAME]` table.** `codex --profile alpha` reads `$CODEX_HOME/config.toml`, then layers `$CODEX_HOME/alpha.config.toml` over it. The `[profiles.name]` table and the top-level `profile =` selector were removed in Codex 0.134.0. Verified against the locally installed `codex-cli 0.154.0` and the [configuration reference](https://developers.openai.com/codex/config-reference); see also [openai/codex#24858](https://github.com/openai/codex/issues/24858).

The proxy's existing output is already correct for this scheme: it writes a standalone `bedrock-local.config.toml` and documents `codex --profile bedrock-local`. Giving each preset its own overlay file, `bedrock-local-<preset>.config.toml`, is therefore the natural unit of isolation, and it needs **only a filename change**.

**Only one profile overlays per Codex invocation.** Each preset's overlay file can carry its own `[model_providers.bedrock-local]` block and its own `model_catalog_json`, because two overlay files are never active at the same time. The provider id does not need to vary, so `codex.ProviderID` and `ProfileTOML` stay as they are and `doctor.validateProfile`'s hard-coded provider constant ([doctor.go:364](../../internal/doctor/doctor.go:364)) remains correct.

**`model_catalog_json` is a single global value, overridable per profile.** Exactly one catalog is live per invocation, which matches one preset per invocation. Per-preset catalog files work.

**A missing profile file is silently ignored.** `codex --profile nope` runs cleanly against the base configuration with no warning. With several presets a typo becomes likely, and the failure mode is that Codex quietly bypasses the proxy — and therefore quietly bypasses the preset's model restriction. This makes `doctor --preset NAME --client codex` materially more important, and the design keeps it checking that the exact expected overlay file exists.

**Codex profile names reject dots and path traversal** and accept letters, digits, hyphens, and underscores. Because a preset name becomes part of a Codex profile name, this bounds the preset character set.

**`amazon-bedrock` is a reserved built-in Codex provider id** permitting only a narrow set of overrides. Keeping the provider id at the constant `bedrock-local` keeps it clear of the reserved list.

## Options and Candidate Seams

### A. Standalone preset files, each a complete configuration

The original shape, before the model catalog was known to be shared. Now clearly wrong: it duplicates every model definition, including pricing and capabilities, across files that must stay in step. Model pricing is exactly the data most likely to drift, and a stale copy silently produces wrong cost reports.

### B. Standalone overlay files layering over shared `config.yaml`

Keeps one file per preset while sharing `models:`. Weakened by needing a two-file read and a merge step, and by deferring cross-reference checks: a preset naming a model that `config.yaml` does not define can only fail once both files are loaded together, and port uniqueness across presets cannot be checked at all without globbing the directory.

### C. A `presets:` block in `config.yaml` — **chosen**

One file, one loader, no merge rules. Every check the design needs is a single pass over one decoded document: allowed aliases exist, client defaults fall inside the allowed set, and no two presets claim the same port. This reuses the validation pattern `Validate()` already applies to `codex.default_model`. `KnownFields(true)` continues to catch typos in preset keys.

The cost is a longer `config.yaml`, which is small: a preset is three or four lines once the models are shared.

### D. Enforcement by narrowing only the generated client catalogs

Would tidy the Codex catalog and the Claude picker without limiting what the proxy serves. Rejected in favor of real enforcement, which turned out to be cheaper: filtering `cfg.Models` restricts the request paths, the model listing, the generated catalogs, and pricing in one move, with no new code in `internal/server`.

### E. Per-invocation `CODEX_HOME`

Codex's strongest isolation unit. Rejected: it relocates authentication, history, and MCP state that nobody asked to separate, and replaces a flag with an environment variable. Overlay files already give per-preset provider, catalog, and model selection.

## Proposed Delta

### Schema

`config.yaml` gains one optional top-level key. Everything above it is unchanged.

```yaml
version: 1

aws:
  profile: WORK
  region: us-east-2

listen: 127.0.0.1:8787        # the unnamed default's port

models:                       # written once, shared by every preset
  luna:  { ... }
  fable: { ... }
  opus:  { ... }

codex:
  default_model: luna

presets:
  work:
    listen: 127.0.0.1:8788
    session_tag: work
    models: [luna, fable]
  cheap:
    listen: 127.0.0.1:8789
    session_tag: cheap
    models: [luna]
```

```go
type PresetConfig struct {
    Listen     string       `yaml:"listen"`
    SessionTag string       `yaml:"session_tag,omitempty"`
    Models     []string     `yaml:"models,omitempty"`
    Codex      CodexConfig  `yaml:"codex,omitempty"`
    Claude     ClaudeConfig `yaml:"claude,omitempty"`
}
```

`models:` appears at two levels with two shapes — a map of definitions at the top, a list of names inside a preset. The nesting disambiguates it and it reads naturally as "which models", so the name is kept.

A preset does **not** override `aws:` or `reporting.directory`. Presets share one AWS target and one report tree. Neither was asked for, and both are cheap to add later if needed.

### Resolution

One new function carries the whole feature:

```go
func (c Config) Resolve(preset string) (Config, error)
```

It returns an effective `Config` with `Models` filtered to the preset's allowed aliases, `Listen` replaced, the client defaults replaced where the preset sets them, and `Presets` cleared. An empty preset name returns the configuration unchanged.

```
config.yaml ──LoadFile──> Config{Models: all, Presets: {...}}
                              │
                              ├── Resolve("")      ──> Config{Models: all,          Listen: 8787}
                              └── Resolve("cheap") ──> Config{Models: {luna},       Listen: 8789}
                                                          │
                          server · accounting · codex · claude · doctor
                              (unchanged — they only ever see a Config)
```

This is why `internal/server` needs no change. A request for `opus` under preset `cheap` misses the filtered map at [chat.go:209](../../internal/server/chat.go:209) and takes the existing unknown-model path, which returns `400 invalid_request_error` with `unknown model "opus"; configured models: luna` — so the refusal also tells the caller what that preset does allow. The three generation endpoints share this path; the separate `404 model_not_found` at [server.go:232](../../internal/server/server.go:232) belongs to the `/v1/models/{alias}` lookup route. `/v1/models` lists two entries under `work` and one under `cheap`. The generated Codex catalog and Claude picker contain only the allowed aliases for the same reason.

### Validation, all at load time

Because presets live in the file that is already being validated, every one of these is a single pass with a precise error:

- A preset name matches `^[a-z0-9][a-z0-9_-]*$`, at most 32 characters. Lowercase only, because the name becomes a filename on a case-insensitive filesystem; no dots or separators, so it cannot escape the clients directory and is always a valid Codex profile name.
- Every alias in a preset's `models:` list is defined in the top-level `models:` map, and the list has no duplicates. An omitted list means all models; an empty list is an error rather than a proxy that serves nothing.
- The effective `codex.default_model`, `claude.default_model`, and `claude.subagent_model` for each preset fall inside that preset's allowed set. A top-level `codex.default_model: opus` with a preset that excludes `opus` is rejected unless the preset sets its own — this is the one place the shared catalog and per-preset narrowing genuinely interact, and catching it at load time is much better than a confusing failure inside `configure codex`.
- `listen` is required in every preset, and no two presets share one. A preset may not reuse the top-level `listen` either, since the unnamed default remains startable. This is what makes simultaneous presets safe by construction rather than by a bind-time error. It does mean the unnamed default reserves a port even for an operator who only ever runs presets; that cost is accepted deliberately, because the alternative is a `net.Listen` failure that names an address rather than the two configurations that claim it.
- A `session_tag` is validated by the existing `accounting.NormalizeSessionTag` ([session.go:107](../../internal/accounting/session.go:107)), so configuration and CLI reject the same inputs.

### CLI

`--preset NAME` is added to each of the five flag sets, not placed before the subcommand keyword, because dispatch reads the first positional argument:

```
bedrock-proxy --preset work
bedrock-proxy report --preset work --start 2026-09-01 --stop 2026-09-08
bedrock-proxy configure codex --preset work
bedrock-proxy configure claude --preset work
bedrock-proxy doctor --preset work --client codex
```

`--preset` and `--config` **compose**: `--config` chooses the file, `--preset` chooses an entry inside it. This is a change from the earlier standalone-file design, where the two were alternative ways to name the same thing and combining them was a usage error.

`--preset` is a `singleValue`, the existing flag type that rejects a repeated flag ([main.go:42](../../cmd/bedrock-proxy/main.go:42)), because last-wins on a flag that selects an entire runtime configuration is a bad failure mode.

An unknown preset fails with exit 2 and lists the names actually defined in the file.

`--session-tag` overrides a preset's `session_tag` for one run, matching how `--report-dir` overrides `reporting.directory`.

### Generated client artifacts

| Artifact | No preset (unchanged) | With preset `work` |
| --- | --- | --- |
| Codex catalog | `~/.config/bedrock-proxy/clients/codex/models.json` | `~/.config/bedrock-proxy/clients/work/codex/models.json` |
| Codex overlay file | `$CODEX_HOME/bedrock-local.config.toml` | `$CODEX_HOME/bedrock-local-work.config.toml` |
| Codex launch | `codex --profile bedrock-local` | `codex --profile bedrock-local-work` |
| Claude settings | `~/.config/bedrock-proxy/clients/claude/settings.json` | `~/.config/bedrock-proxy/clients/work/claude/settings.json` |
| Codex provider id | `bedrock-local` | `bedrock-local` (unchanged) |

`--catalog` and `--settings` keep overriding the derived paths. No `--codex-profile` flag is added; the overlay path is derived and printed, as today.

### Reporting

Presets share the report tree. `accounting.SessionOptions` gains the preset name, written into `session.json` and each event beside the existing session tag. `reports` gains a preset breakdown alongside the existing session-tag breakdown ([reports.go:37](../../internal/reports/reports.go:37)), and `report --preset NAME` filters to one preset.

The preset name is recorded independently of the session tag rather than derived from it. The tag is operator free-text that `--session-tag` can override per run, so it is not a reliable key.

**Report pricing must use the unfiltered model set.** `resolveReportCommandInputs` currently hands `cfg.Models` to `reports.Generate` for repricing ([main.go:522](../../cmd/bedrock-proxy/main.go:522)). If `report --preset cheap` were given the *resolved* configuration, every session that used a model outside `cheap` would price as unavailable — a silent, wrong cost report. So `report` resolves the preset only to obtain the filter, and passes the top-level `models:` map for pricing. This is the one place where the "resolve to a plain Config and forget presets existed" rule does not hold, and it is deliberate.

### Startup output

Text startup gains a `Preset:` line when one is in use. The JSON startup record gains a `preset` field. The existing `profile` field keeps meaning the AWS profile; the two are separate keys with separate names, which is why the new concept is not called a profile.

### `bedrock-proxy preset create`

Hand-writing a preset means picking a port nothing else claims and naming models exactly as the catalog spells them. The command does both.

```
bedrock-proxy preset create [--config PATH] [--name NAME] [--listen HOST:PORT]
                            [--models a,b] [--session-tag TAG]
                            [--codex-model ALIAS] [--claude-model ALIAS] [--dry-run]
```

It dispatches like `configure`: `preset` as the first positional argument, `create` as the second, leaving room for `preset list` and `preset remove` later.

**Port scan.** Two facts combine. A port is *claimed* if the top-level `listen` or any existing preset uses it — known from the parsed file. A port is *busy* if `net.Listen` on it fails right now. The scan walks upward from the top-level `listen` port, over a bounded range of 100, and proposes the first port that is neither. It probes the loopback host the top-level `listen` already uses, so the proposal matches the operator's own convention and passes `ValidateListenAddress` ([config.go:254](../../internal/config/config.go:254)). If the whole range is exhausted the command fails and names the range rather than proposing something unusable.

A successful probe is advisory, not a reservation. Another process can take the port between creating the preset and starting the proxy. That is acceptable because the proxy already fails clearly at bind; the scan removes the common case, not the race.

The scan is also what makes the strict port rule cheap. Requiring every preset's port to differ from the top-level `listen` costs an operator nothing when the next free port is chosen for them.

**Prompting.** Interactivity is decided once in `main()` from `os.Stdin`'s mode, using `os.Stat` rather than a new dependency, and passed down as a boolean. `run` gains a stdin reader alongside its existing writers. Separating "is a terminal" from "where input comes from" means the prompt loop is exercised in tests with an ordinary `strings.Reader`, with no pseudo-terminal.

On a terminal, each unanswered setting is prompted with a default in brackets and an empty answer accepts it. Off a terminal, a missing required setting is a usage error naming the flag that supplies it. A flag always wins over a prompt, so the command is fully scriptable.

Questions, in order:

| Prompt | Default offered | Checked against |
| --- | --- | --- |
| Name | none, required | the name pattern; must not already exist |
| Port | the scanned free port | loopback, unclaimed, and probed again |
| Models | all catalog aliases | each alias exists in `models:`; no duplicates |
| Session tag | the preset name | `accounting.NormalizeSessionTag` |
| Codex / Claude default | the top-level default | must lie inside the chosen model subset |

The model prompt lists the catalog aliases with their display names, because the whole point is that the operator is narrowing a set they may not have memorised.

Offering the preset name as the session-tag default does not contradict the decision against an implicit tag. The operator sees the value and confirms it; nothing is applied silently, and clearing the prompt leaves the preset untagged.

The last two prompts appear only when the top-level client default falls outside the chosen subset. That is exactly the validation rule from the schema section, surfaced at the moment it can be answered instead of as a rejection afterwards.

**Validation before writing.** The command assembles the candidate configuration in memory, adds the new preset, and runs the same `Validate()` the loader runs. Nothing is written unless it passes, so `preset create` cannot produce a file that fails to load.

**Node-anchored insert.** The file is not re-serialized. Prototyping showed that finding the insertion point by searching the text is unsafe, so the anchor comes from the parse instead — which is free, because the command already parses the file to validate it.

`yaml.Unmarshal` into a `yaml.Node` yields the document's root mapping. The top-level `presets` key node gives the authoritative line, and its value node gives three things no text scan can determine correctly:

| From the node | Used for |
| --- | --- |
| key `.Line` | where the block starts |
| value `.Content[0].Column` | the indentation to render the new preset at |
| value `.Style` and `.Kind` | whether the block can be extended at all |

Only the *end* of the block is found textually: scan forward from the first child's line while lines are blank or indented, and splice the rendered preset in after it. Everything else is copied byte for byte, so comments, blank lines, quoting, key order, line endings, and a missing final newline all survive.

Three cases branch:

- **`presets:` is a block mapping with entries** — insert at the end of the block, at the first child's column.
- **`presets:` is absent** — append a fresh `presets:` block at the end of the file.
- **`presets:` is present but empty** — insert directly under the existing key at indent 2. This case must not be folded into "absent": appending a second `presets:` key produces a duplicate mapping key and the file stops parsing entirely.
- **`presets:` is a flow mapping** (`presets: {work: {...}}`) — decline, and print the block for the operator to paste. A scanner that guesses at unusual YAML would corrupt a file the operator maintains by hand.

The command only ever adds a preset. Editing or removing one needs real structural rewriting, which is why `preset remove` is not in this change.

The write is a temporary file in the same directory followed by a rename, the pattern already used by `writeReport` ([main.go:537](../../cmd/bedrock-proxy/main.go:537)) and `writeCatalogAtomic`, preserving the original file mode. The temporary file is parsed and validated before the rename, so a failed insert leaves the original untouched.

The prototype's fixtures should become the test table: `presets` last and not last, comments inside and immediately after the block, blank lines between entries, four-space indentation, no trailing newline, CRLF endings, an empty `presets:` with and without a comment, and four adversarial files where the text `presets:` appears in a comment, in a block scalar, inside a multi-line quoted scalar at column zero, and as a nested key under `codex:`.

`--dry-run` always prints the block and the insertion point and writes nothing.

**Output** names the preset, the chosen port, the allowed models, the file written, and the follow-up commands: `configure codex --preset NAME` and `doctor --preset NAME`. Like `report`, `configure`, and `doctor`, this command is text-only; `--log-format json` is not offered, because a prompt loop has no JSON form.


## Transition and Coexistence

The unnamed default and named presets coexist permanently. This is not a migration and there is no temporary path to remove.

`presets:` is optional. A `config.yaml` without it validates and behaves exactly as today, and without `--preset` every path, filename, and identifier is byte-for-byte unchanged: `clients/codex/models.json`, `clients/claude/settings.json`, `bedrock-local.config.toml`, and `codex --profile bedrock-local`. The configuration version stays at `1`, because adding an optional key breaks no existing file.

Adding a `presets:` block can newly fail validation of a previously valid file in one case: if the top-level `listen` is the default `127.0.0.1:8787` and a preset also claims it. The error names both and says the top-level value is the unnamed default's port.

Session artifacts written before this change lack the preset field. `reports` treats a missing preset as unattributed, matching its existing handling of a missing session tag.

## Decisions

| Decision | Choice | Rationale | Confidence | Reopen if |
| --- | --- | --- | --- | --- |
| Name of the concept | `preset` | "Profile" already means the AWS credential set and the Codex CLI profile; a third meaning would make startup output and diagnostics ambiguous. `preset` appears nowhere in this repository or in AWS, Codex, or Claude vocabulary. | medium | a client tool introduces its own "preset" |
| Storage | a `presets:` block in `config.yaml` | Models are shared, so a preset is three or four lines. One file means one loader, no merge rules, and every cross-reference and the port-uniqueness rule validate in the pass that already runs. | high | presets grow enough to need per-preset `aws:` or their own lifecycle |
| Enforcement | filter the effective `Models` map | Every consumer already keys off `cfg.Models`, so one filter restricts routing, the model listing, generated catalogs, and pricing at once, with no change to `internal/server`. Confirmed in implementation: all three generation endpoints refuse a blocked alias before any AWS request. | high | a preset ever needs to *see* a model it may not call |
| Preset resolves to a plain `Config` | yes | Keeps the concept out of five packages. The only exception is report pricing, which is explicit. | high | a downstream consumer genuinely needs the preset identity beyond reporting |
| Report pricing model set | unfiltered | Pricing a historical session against a narrowed catalog silently reports cost as unavailable. | high | reports are ever scoped so tightly that off-preset sessions cannot appear |
| Port uniqueness | rejected at load time, including against the top-level `listen` | All presets are in one file, so this is checkable and deterministic. Participant: clear errors are better than a saved port. The cost is reserving one port for the unnamed default, and `preset create`'s port scan means an operator never pays it by hand; the benefit is that a port conflict is impossible to reach at runtime. | high | the unnamed default stops being startable |
| Codex isolation | per-preset overlay filename | Codex layers `$CODEX_HOME/<name>.config.toml` over the base configuration, so the overlay file is its natural per-setup unit. Costs one filename change. | high | Codex changes profile resolution again |
| Codex provider id | unchanged constant | Only one overlay is active per invocation, so ids never coexist. Keeps `ProfileTOML` and `doctor.validateProfile` untouched and clear of the reserved `amazon-bedrock` id. | high | the proxy emits provider blocks into the operator's base `config.toml` instead of an overlay file |
| `--preset` with `--config` | compose | They name different things now: the file, and an entry inside it. | high | — |
| Preset as implicit session tag | no | The preset name is recorded separately in the manifest, so an implicit tag is redundant and would silently change report contents. | medium | operators set `session_tag` to the preset name every time |
| Per-preset `aws:` | not included | Not requested, and every preset sharing one model catalog implies one Bedrock target. | medium | a preset needs a different region or account |
| `preset create` writes the file | yes, by node-anchored insert | The operator's `config.yaml` is hand-maintained and comment-heavy. Inserting text preserves it byte for byte outside the added lines; re-serializing the whole document would normalise indentation, quoting, and blank lines. The insertion point comes from the parsed node rather than a text search — see the prototype findings. Works only because the command adds and never edits. | high | `preset remove` or `preset edit` is added, which needs real structural rewriting |
| Unusual YAML in `presets:` | decline and print the block | Decided from the value node's kind and style, not guessed from text. Guessing would corrupt a file the operator owns. | high | the proxy takes ownership of `config.yaml` formatting |
| Interactivity | prompt on a TTY, flags otherwise | Keeps the command scriptable and testable without a pseudo-terminal, and preserves the existing "flags in, exit codes out" contract for every non-interactive use. | high | — |
| TTY detection | `os.Stdin` mode via `os.Stat` | No new dependency, in a project with a deliberately small `go.mod`. | medium | detection proves unreliable on a target platform |
| Validation before write | assemble in memory, run the loader's `Validate()`, then validate the temp file before renaming | Makes "`preset create` cannot produce a file that fails to load" true rather than aspirational, and reuses every rule rather than restating it. | high | — |
| `--log-format json` for `preset create` | not offered | A prompt loop has no JSON form, and `report`, `configure`, and `doctor` are already text-only. | high | the command becomes fully non-interactive |

## Research and Prototype Findings

The Codex research changed the design twice. It established that overlay files, not `[profiles.NAME]` tables, are how `--profile` works on Codex 0.134.0 and later, which made the per-preset overlay filename the obvious seam. It then showed that only one overlay is active per invocation, which removed the need to vary the provider id and cut the `internal/codex` and `internal/doctor` changes to a single path function each.

It also surfaced a hazard worth designing around: Codex silently ignores a missing profile file, so a mistyped preset name makes Codex bypass the proxy — and therefore the preset's model restriction — without warning. `doctor` is the mitigation.

Findings are from the [Codex configuration reference](https://developers.openai.com/codex/config-reference) and [advanced configuration](https://developers.openai.com/codex/config-advanced), verified empirically against `codex-cli 0.154.0`. The character rules for profile names come from documentation plus black-box probing rather than the validation source, so the 32-character preset limit is this design's own conservatism, not a Codex constraint.

No prototype was built. The remaining uncertainty was documentary, and reading Codex's behavior answered it.

### Prototype: the preset insert

A disposable prototype in a scratch module implemented three strategies against sixteen hand-written fixtures, checking for each that the output parses, that the new preset decodes correctly, that every original line survives in order, and that declines happen exactly where expected. It changed the design three times.

**The text-search anchor was wrong, and failed silently.** Ten of sixteen fixtures failed. The damaging one: taking the block's indentation from the *last* line of the block rather than the first child.

```yaml
# naive text scan                    # node-anchored
presets:                             presets:
  # the main work setup                # the main work setup
  work:                                work:
    listen: 127.0.0.1:8788               listen: 127.0.0.1:8788
    models: [luna]                       models: [luna]
    cheap:              <-- wrong      cheap:
      listen: ...                        listen: ...
```

The result is valid YAML that parses cleanly; `cheap` has simply become a field of `work`. `KnownFields(true)` would catch it at the validate-before-rename step, so the file would not be corrupted — but the design would have been relying on a last-line safety net rather than on being correct. Anchoring on the parsed node removes the class of error.

**An empty `presets:` key was an unhandled case that produced an unparseable file.** Treating a present-but-null `presets:` as absent appends a second `presets:` key; the file then fails to load with a duplicate-key error. It needs its own branch. This was a genuine gap in the written design, not a prototype artifact.

**The adversarial cases confirmed the anchor must come from the parse.** The text `presets:` appearing in a comment, in a block scalar, or as a nested key under `codex:` is handled correctly by a column-zero text check. But a multi-line double-quoted scalar can place `presets:` at column zero as string content, and there the text scanner declined a file it should have edited. Only the node anchor gets all four right.

Insert-at-end and insert-at-top both passed all sixteen fixtures once anchored, so ordering is a free choice; the design keeps insert-at-end so entries stay in the order they were created.

## Remaining Design Questions

1. **Should a preset be able to override `aws:`?** Sharing one model catalog implies one Bedrock target, so this is out of scope now. It would matter if the same models were served from two regions or two accounts.
2. **Should `configure codex` write the overlay file instead of printing it?** Printing TOML to paste is tolerable for one setup and repetitive across presets, and the printed path is now preset-specific so a mistake is easier. This is a separate decision about whether the proxy writes into `$CODEX_HOME` at all, which it deliberately does not do today.
