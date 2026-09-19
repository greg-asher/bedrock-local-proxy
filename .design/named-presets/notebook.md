# Named Presets Design Notebook

## Current Position

A **preset** is a named entry in `config.yaml` setting a port, a session tag, and which of the shared models it allows. `bedrock-proxy --preset work` runs under it; all five subcommands take the flag.

The seam is one function in `internal/config`: `Resolve(preset)` returns an ordinary `config.Config` with `Models` filtered and `Listen` replaced. Nothing downstream learns presets exist, so model restriction enforces itself across routing, the model listing, generated catalogs, and pricing at once.

Most recent change: **implemented** on branch `feat/named-presets`. Full suite green; all supported cross-build targets compile. Three things implementation corrected, recorded under Research and Prototypes. Before that, prototyped the preset insert. The text-search anchor in the previous draft was wrong — it took indentation from the last line of the block and silently nested the new preset inside the previous one — so the insert is now anchored on the parsed `yaml.Node`. A present-but-empty `presets:` key was also an unhandled case that produced an unparseable file. Before that, added `bedrock-proxy preset create` — scans for a free port, prompts for the settings on a terminal, takes flags otherwise, and inserts the preset into `config.yaml` as text so the operator's comments survive. Before that, the participant confirmed the strict port rule — the top-level `listen` must differ from every preset, because a clear load-time error beats a saved port. Before that, the participant said all presets share one model catalog and only narrow it. That reversed the storage decision — models can no longer be duplicated across standalone files — and it turned out to make the whole design smaller and stricter. Design republished.

## Requested Change

An operator packages a setup under a short name and launches it directly. A preset sets the session tag, the port, and the allowed models. All presets share one model catalog.

Scenarios:

1. `bedrock-proxy --preset work` binds that port, tags the session report, serves only its models, and names the preset at startup.
2. A second preset runs at the same time. Neither overwrites the other's generated client files.
3. A client asks a preset for a model it does not allow. Refused.
4. `configure codex --preset work` produces metadata without disturbing another setup.
5. `report --preset work` reports that preset only.
6. `bedrock-proxy preset create` proposes a free port, asks for the name, models, and tag, and adds the preset to `config.yaml`.

## Starting Sources

- `cmd/bedrock-proxy/main.go` — subcommand dispatch, every flag set, report pricing inputs
- `internal/config/config.go` — `DefaultPath`, `Validate`, `DefaultListen`, `ResolveReportDirectory`
- `internal/server/{chat,messages,responses,server}.go` — the `cfg.Models` lookups and the `model_not_found` path
- `internal/codex/catalog.go` — `ProviderID`, `DefaultProfileName`, `DefaultCatalogPath`, `ProfileTOML`
- `internal/claude/settings.go` — `DefaultSettingsPath`
- `internal/doctor/doctor.go` — `DefaultProfilePath`, `validateProfile`
- `internal/accounting/session.go` — session naming, retention, `NormalizeSessionTag`
- `internal/reports/reports.go` — breakdowns and aggregation
- `cmd/bedrock-proxy/main_test.go` — arg-parsing conventions and the helper-process harness
- `README.md`, `config.example.yaml`
- Prior work: `.design/session-reporting/notebook.md`

## Relevant Current Behavior

Dispatch reads the first positional argument (main.go:64). A subcommand keyword must come first; a flag before it is parsed as a proxy flag. All five subcommands take `--config`, falling back to one machine-wide `$HOME/.config/bedrock-proxy/config.yaml` (config.go:74).

**`cfg.Models` is already the single gate for model access.** Every request path looks the alias up there (chat.go:209, messages.go:141, responses.go:146) and an unknown alias already yields a proper `model_not_found` (server.go:226). The `/v1/models` listing, catalog generation, the Claude picker, and accounting pricing read the same map. Filtering it is therefore sufficient enforcement, and `internal/server` needs no change.

**`Validate()` already cross-checks aliases** against `c.Models` for `codex.default_model` (config.go:176) and both Claude selections (config.go:189). Preset validation reuses the pattern.

**The session tag exists only as a CLI flag** (main.go:257) and is never read from configuration.

**Nothing in this binary is interactive today.** `run(args, stdout, stderr)` takes no stdin, there is no TTY detection, and `go.mod` has four direct dependencies. `preset create` is the first command that reads input.

**This binary does not write files the operator owns.** `configure codex` prints TOML for the operator to paste and never touches Codex configuration; `configure claude` never modifies `~/.claude/settings.json`. `config.yaml` is our own file, but it is hand-maintained and comment-heavy — `install.sh` copies the comment-dense `config.example.yaml` into place — so writing to it still has to preserve what the operator wrote.

Collisions between simultaneous presets:

- `codex.DefaultCatalogPath` = `<dir of config>/clients/codex/models.json` (catalog.go:310) and `claude.DefaultSettingsPath` = `clients/claude/settings.json` (settings.go:255). With presets inside one `config.yaml`, these are now a *guaranteed* collision, not a conditional one.
- `codex.DefaultProfileName` is the constant `bedrock-local` (catalog.go:25); `doctor.DefaultProfilePath` ignores the config path and returns one machine-wide `$CODEX_HOME/bedrock-local.config.toml` (doctor.go:255). `configure codex` sends the operator there (main.go:203). A second setup overwrites the first, and `doctor` then reads the survivor plus the catalog path recorded inside it (doctor.go:125), silently validating the wrong setup.
- `config.DefaultListen` is `127.0.0.1:8787` (config.go:20).

Session directories do **not** collide: timestamp, PID, random suffix, `os.Mkdir` (session.go:150). Only the shared report parent mixes usage.

**Report pricing hands `cfg.Models` to `reports.Generate`** (main.go:522). This is the one place a filtered map would be actively wrong.

## Affected Surface

`internal/config` (schema, validation, `Resolve`, free-port scan, textual insert), `cmd/bedrock-proxy` (`--preset` on five flag sets, the `preset create` subcommand, a stdin reader and interactivity flag on `run`, startup output, unfiltered pricing map for `report`), `internal/codex` and `internal/claude` and `internal/doctor` (one path function each), `internal/accounting` (record preset in manifest and events), `internal/reports` (breakdown and filter), plus `README.md` and `config.example.yaml`.

`internal/server`, `internal/transport`, `internal/pricing`, and `install.sh` do not change.

## External Research

**Q1. How does `codex --profile NAME` resolve, and can several local providers coexist?** — resolved.

`--profile NAME` layers `$CODEX_HOME/NAME.config.toml` over the base `config.toml`. The `[profiles.NAME]` table and the top-level `profile =` selector were removed in Codex 0.134.0. Verified against the installed `codex-cli 0.154.0`. Sources: [config reference](https://developers.openai.com/codex/config-reference), [config advanced](https://developers.openai.com/codex/config-advanced), [openai/codex#24858](https://github.com/openai/codex/issues/24858).

Code implications:
- The repo's existing output already fits this scheme. My earlier note about a `bedrock-local.config.toml` / `--profile bedrock-local` inconsistency was wrong; there is none.
- Per-preset overlay filename `bedrock-local-<preset>.config.toml` is the natural isolation unit. Only `doctor.DefaultProfilePath` changes.
- Only one overlay is active per invocation, so `codex.ProviderID` stays constant and `ProfileTOML` and `doctor.validateProfile` are untouched.
- `model_catalog_json` is one global value, overridable per profile — one catalog live per invocation, matching one preset per invocation.
- Hazard: a missing profile file is silently ignored, so a mistyped preset makes Codex bypass the proxy and therefore the preset's model restriction, with no warning. `doctor` is the mitigation.
- Constraints: profile names reject dots and traversal; `amazon-bedrock` is a reserved built-in provider id, and `bedrock-local` is clear of it.

Uncertainty: the profile-name character rules come from docs plus black-box probing, not the validation source. The 32-character preset limit is this design's own conservatism.

**Q2. Per-preset `CODEX_HOME` instead?** — resolved, rejected. It relocates authentication, history, MCP state, and caches, which nobody asked to separate, and needs an environment variable before every `codex` run.

## Domain Model

Three things are called "profile": the AWS credential set (`aws.profile`), the Codex CLI profile (`codex --profile`), and the requested concept. The third is named **preset** — free of collisions in this repository and in AWS, Codex, and Claude vocabulary.

Terms:

- **Model catalog** — the top-level `models:` map. Written once. Carries targets, pricing, and capabilities.
- **Preset** — a named narrowing of the catalog plus a port and a session tag.
- **Effective configuration** — what `Resolve(preset)` returns: an ordinary `Config`, indistinguishable downstream from a hand-written one.

Invariants:

- A model is defined exactly once. A preset may only name models that exist.
- Two presets never bind the same port; enforced at load time, not at bind time.
- A preset's effective client defaults lie inside its allowed set.
- A preset name is safe as a path segment and as a Codex profile name.
- With no preset, every path, filename, and identifier is byte-for-byte what it is today.
- Historical cost reporting prices against the full catalog, never a preset's narrowing.
- A command that writes `config.yaml` leaves it loadable, or leaves it untouched.

## Candidate Seams and Options

- **A. Standalone complete config per preset.** The original shape. Dead once the catalog is shared: it duplicates pricing and capabilities across files that must stay in step, and a stale copy silently produces wrong cost reports.
- **B. Standalone overlay files over a shared `config.yaml`.** Keeps one file per preset but needs a two-file read and merge, defers cross-reference checks, and cannot check port uniqueness without globbing a directory.
- **C. A `presets:` block in `config.yaml`** — chosen. One file, one loader, no merge rules; every cross-reference and the port-uniqueness rule validate in the pass that already runs. Cost is a longer config file, which is small because a preset is three or four lines.
- **D. Narrow only the generated client catalogs.** Advisory, not enforced. Rejected because real enforcement turned out cheaper.
- **E. Per-preset `CODEX_HOME`.** Rejected; see research.

## Proposed Delta

Full version in `docs/design/named-presets-change-design.md`.

- New optional top-level `presets:` map. `PresetConfig{Listen, SessionTag, Models []string, Codex, Claude}`. No `aws:` or `reporting.directory` override.
- `Resolve(preset) (Config, error)` filters `Models`, replaces `Listen` and any client defaults, clears `Presets`. Empty name returns the config unchanged.
- Load-time validation: name matches `^[a-z0-9][a-z0-9_-]*$` (≤32 chars); every allowed alias exists and is unique; an empty list is an error and an omitted list means all; effective client defaults lie inside the allowed set; `listen` is required, unique across presets, and distinct from the top-level `listen`; `session_tag` passes `NormalizeSessionTag`.
- `--preset` on all five flag sets, inside each set rather than before the subcommand keyword. It **composes** with `--config` — the file versus an entry inside it. Uses the existing `singleValue` type. Unknown preset exits 2 listing the defined names. `--session-tag` overrides a preset's tag.
- Artifacts at `clients/<preset>/codex/models.json` and `clients/<preset>/claude/settings.json`; Codex overlay `$CODEX_HOME/bedrock-local-<preset>.config.toml`, launched as `codex --profile bedrock-local-<preset>`. Provider id unchanged.
- Session manifest and events record the preset name, independent of the operator's tag. `report` gains a preset breakdown and `--preset` filter, and passes the **unfiltered** `models:` map for pricing.
- Startup text gains `Preset:`; the JSON startup record gains `preset` beside the existing `profile` key, which keeps meaning the AWS profile.
- `bedrock-proxy preset create`, dispatched like `configure`, leaving room for `preset list` and `preset remove`:
  - Port scan combines *claimed* (top-level `listen` plus every preset, from the parsed file) with *busy* (`net.Listen` probe now), walking up from the top-level port over a bounded range of 100 on the loopback host the config already uses. The probe is advisory, not a reservation — the proxy still fails clearly at bind. The scan is what makes the strict port rule cost an operator nothing.
  - Interactivity decided once in `main()` from `os.Stdin`'s mode via `os.Stat`, no new dependency, passed down as a boolean; `run` gains a stdin reader. Separating "is a terminal" from "where input comes from" makes the prompt loop testable with a `strings.Reader` and no pty.
  - Prompts in order: name, port (default = scanned), models (default = all, listed with display names), session tag (default = the preset name), and the Codex/Claude default only when the top-level default falls outside the chosen subset. A flag always beats a prompt; off a terminal a missing required setting is a usage error naming its flag.
  - Offering the preset name as the session-tag *default* does not contradict the no-implicit-tag decision: the operator sees and confirms it, and clearing the prompt leaves the preset untagged.
  - Candidate config assembled in memory and run through the loader's `Validate()`; nothing is written unless it passes.
  - Node-anchored insert. The file is already parsed for validation, so the `yaml.Node` is free. Key node `.Line` gives where the block starts; value node `.Content[0].Column` gives the render indentation; value `.Kind`/`.Style` decides whether it can be extended. Only the block *end* is textual: scan forward from the first child while blank or indented. Everything else copied byte for byte — comments, blank lines, quoting, key order, line endings, missing final newline.
  - Four branches: block mapping with entries → insert at end at the first child's column; absent → append a fresh block; **present but empty → insert under the existing key at indent 2** (folding this into "absent" appends a duplicate `presets:` key and the file stops parsing); flow mapping → decline and print the block.
  - Temp file plus rename preserving the original mode; the temp file is parsed and validated before the rename.
  - `--dry-run` prints and writes nothing. Text-only output; no `--log-format json`, since a prompt loop has no JSON form.

## Transition States

Permanent coexistence, not a migration. `presets:` is optional; version stays at `1`. Without `--preset`, nothing moves.

One way a previously valid file can newly fail: the top-level `listen` is the default `127.0.0.1:8787` and a preset also claims it. The error names both.

Pre-existing session artifacts have no preset field and report as unattributed, matching existing handling of a missing session tag.

## Decisions

| Decision | Choice | Confidence | Reopen if |
| --- | --- | --- | --- |
| Term | `preset` | medium | a client tool introduces its own "preset" |
| Storage | `presets:` block in `config.yaml` | high | presets need per-preset `aws:` or their own lifecycle |
| Enforcement | filter the effective `Models` map | high | a preset must *see* a model it may not call |
| Preset resolves to a plain `Config` | yes | high | a downstream consumer needs preset identity beyond reporting |
| Report pricing model set | unfiltered | high | off-preset sessions can never appear in a report |
| Port uniqueness | rejected at load time, including vs. top-level `listen`; participant chose the clear error over the saved port | high | the unnamed default stops being startable |
| Codex isolation | per-preset overlay filename; provider id unchanged | high | Codex changes profile resolution again |
| `--preset` with `--config` | compose | high | — |
| Preset as implicit session tag | no | medium | operators always set `session_tag` to the preset name |
| Per-preset `aws:` | not included | medium | a preset needs a different region or account |
| `preset create` writes `config.yaml` | yes, node-anchored insert | high | `preset remove`/`edit` is added, needing structural rewriting |
| Unusual YAML in `presets:` | decline, decided from the value node's kind and style | high | the proxy takes ownership of config formatting |
| Interactivity | prompt on a TTY, flags otherwise | high | — |
| TTY detection | `os.Stdin` mode via `os.Stat`, no new dependency | medium | detection proves unreliable on a target platform |
| Validation before write | in-memory `Validate()`, then validate the temp file before rename | high | — |

## Research and Prototypes

Codex research complete; see External Research above.

**Prototype: the preset insert.** Question: can a textual scanner reliably find the insertion point in a hand-maintained `config.yaml`, and reliably decline when it cannot? Disposable scratch module, three strategies against sixteen fixtures, each checked for parseability, correct decode of the new preset, survival of every original line in order, and declining exactly where expected.

Result: **no** for a text-search anchor — ten of sixteen fixtures failed. The damaging failure took the block indentation from the last line of the block, nesting the new preset inside the previous one. That output is valid YAML that parses cleanly, so only `KnownFields(true)` at the validate-before-rename step would have caught it; the design would have been leaning on a safety net instead of being correct.

Two further findings:
- A present-but-empty `presets:` key, treated as absent, appends a duplicate key and the file stops parsing. Genuine gap in the written design, now its own branch.
- Adversarial files place the text `presets:` in a comment, in a block scalar, as a nested key under `codex:`, and — the one that matters — at column zero inside a multi-line double-quoted scalar. A column-zero text check gets the first three right and the fourth wrong. Only the node anchor gets all four.

Insert-at-end and insert-at-top both passed all sixteen once anchored, so ordering is free; insert-at-end keeps creation order.

Design effect: the insert is node-anchored, the empty-key case is explicit, and the prototype's fixtures become the test table. Remaining uncertainty: none that blocks implementation. The prototype code is disposable and lives only in the session scratchpad.

## Active Change Frontier

Empty. Every question that could change the proposed delta is resolved.

Deferred, deliberately out of scope:

1. `preset list` and `preset remove`. The dispatch shape leaves room for both. `remove` needs real structural YAML rewriting, which the textual insert deliberately does not do.
2. Should a preset override `aws:`? Out of scope while one catalog implies one Bedrock target.
3. Should `configure codex` write the overlay file rather than print it? Now that `preset create` writes `config.yaml`, the "we never write the operator's files" stance is narrower than it was, so this is worth revisiting — but `$CODEX_HOME` is a third party's directory, which `config.yaml` is not.

## Decision Map

- Status: not needed
- Path: none
- Destination: —
- Return condition: —

## Best Next Move

The design is complete and coherent, including `preset create`. Nothing further needs code tracing, research, a prototype, or participant judgment. A resuming session should read `docs/design/named-presets-change-design.md` and treat the three deferred items above as separate changes, not gaps in this one.

The insert has been prototyped and the algorithm is settled; carry its fixtures into `internal/config` as the test table.
