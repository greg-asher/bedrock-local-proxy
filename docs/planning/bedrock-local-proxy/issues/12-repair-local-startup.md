# Repair local startup evidence and endpoint reporting

## Useful outcome

The local startup slice has reliable CLI behavior, usable startup instructions, and tests that prove its observable contract.

## What changes

Make `--version --log-format json` emit one valid JSON record while preserving plain text output in the default mode. Ensure startup reports the actual bound listener address, or reject port `0` before binding; it must never advertise an unusable `:0` URL.

Add command-level tests for version output, default and explicit configuration paths, startup text and JSON, occupied-port failure, loopback rejection, and signal shutdown. Use temporary home/config directories and an ephemeral test port without touching the developer’s real configuration.

Add concise repository startup instructions covering Go as the build prerequisite, `./install.sh` when issue 11 lands, `~/.config/bedrock-proxy/config.yaml`, `--config`, `aws sso login --profile Halo-Win-Agent-Execution`, and the localhost URL. Keep the model ID as a user-supplied placeholder. Explain that startup proves local binding only; AWS connectivity is proven by a later request.

## Requirements and delivery context

This repair closes the initial-review findings for issue 01. The affected flow is CLI invocation → flag parsing → config selection/validation → listener bind → startup output → HTTP serving/shutdown. Preserve the localhost-only boundary, the user-editable default config path, the actual profile and region in the example, and the no-secret/no-content logging rule.

Do not add an interactive setup wizard, automatic SSO login, shell startup edits, model discovery, or a second configuration system. Tests must be deterministic and must not require AWS access.

## Done when

- JSON mode produces valid JSON for version, startup, and diagnostics; default mode remains human-readable.
- Startup output contains the actual bound `http://127.0.0.1:<port>/v1` address for any accepted configuration, and port `0` is either rejected or reported with its assigned port.
- Command-level tests prove default/explicit config paths, invalid/non-loopback config, occupied-port failure, version without config, startup formats, and SIGINT/SIGTERM shutdown.
- Repository instructions explain the first local startup journey and point to the exact user-editable config path without exposing credentials.
- Existing config/server tests and `go test ./...` plus `go build ./...` pass with temporary caches when host caches are unavailable.
