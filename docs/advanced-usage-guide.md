# Bedrock Local Proxy: Advanced User Guide

Bedrock Local Proxy is a loopback-only, AWS-signed gateway for Codex, Claude Code, and OpenAI-compatible clients. It maps stable local aliases to Bedrock model or inference-profile IDs, supports streaming and tools, and records usage metadata without storing prompts or responses.

## Recommended operating pattern

1. **Refresh identity and confirm the role.** The proxy uses the AWS SDK credential chain and the profile in `~/.config/bedrock-proxy/config.yaml`; never put access keys in that file.

   ```sh
   aws sso login --profile YOUR_AWS_PROFILE
   aws sts get-caller-identity --profile YOUR_AWS_PROFILE
   ```

2. **Treat aliases as stable client contracts.** Point each alias at an exact model or inference-profile ID. Set explicit `max_tokens`, accurate input/output/cache prices, and authoritative capabilities; `max_tokens` must not exceed `capabilities.max_output_tokens`.

3. **Generate and verify client configuration after every model, capability, listener, default-model, or client-version change.**

   ```sh
   bedrock-proxy configure codex
   bedrock-proxy doctor --client codex
   bedrock-proxy configure claude
   bedrock-proxy doctor --client claude
   ```

   Save Codex's printed TOML at the reported path. Offline `doctor` catches stale files and incompatible metadata. With SSO active and the proxy running, add `--live` to exercise discovery, streaming, usage, reasoning, and tool-call continuation.

4. **Run with attribution and a safety ceiling.**

   ```sh
   bedrock-proxy --session-tag team-a-import --budget 5.00
   ```

   A budget is a per-process guardrail—not an AWS billing limit. It resets on restart, in-flight requests may overshoot it, and requests without reported usage do not increase the total. Every selected model needs resolvable prices; OpenAI streaming callers should send `stream_options.include_usage`.

## Separate workloads with presets

Presets reuse the catalog while assigning a distinct port, model allow-list, defaults, and session tag—useful for teams, cost profiles, or coding versus batch workloads.

```sh
bedrock-proxy preset create --name cheap --models luna --session-tag cheap
bedrock-proxy configure codex --preset cheap
bedrock-proxy doctor --preset cheap --client codex
bedrock-proxy --preset cheap --budget 2.00
codex --profile bedrock-local-cheap
```

A preset is enforced server-side: `/v1/models` lists only allowed aliases, and disallowed requests never reach AWS. Each preset has independent client files and a unique port. Because a mistyped Codex profile can bypass the proxy, run `doctor --preset NAME` before launch.

## Connect clients

| Client | Base URL | Credential | Model |
| --- | --- | --- | --- |
| OpenAI-compatible | `http://127.0.0.1:8787/v1` | API key `local` | Local alias |
| Anthropic Messages | `http://127.0.0.1:8787` | API key `local` | Messages-capable alias |
| Codex | Generated `bedrock-local` profile | Generated local bearer value | Responses-capable alias |
| Claude Code | Generated settings file | Generated local value | Messages-capable alias |

The dummy key only satisfies client validation; it is stripped before the proxy signs upstream requests. Bedrock Runtime Responses does not execute Codex hosted `web_search`; configure search as an MCP tool instead. Client-side tools remain available.

## Observe and report

The terminal summarizes status, endpoint, model, latency, tokens, cache use, and estimated cost. `--log-format json` is machine-readable. Each run also writes owner-only `session.json`, `events.jsonl`, and `summary.json`; these exclude prompts, completions, tool content, credentials, request bodies, and raw upstream errors.

```sh
bedrock-proxy report --start 2026-09-01 --stop 2026-10-01
bedrock-proxy report --preset cheap --start 2026-09-01 --stop 2026-10-01 --format json
```

`--start` is inclusive and `--stop` exclusive. Reports reprice history with the current full catalog and mark cost partial or unavailable when data is missing. Estimates are operational telemetry, not the AWS invoice.

## Fast failure triage

- **Credentials unavailable/expired:** rerun `aws sso login` for the configured profile.
- **Access denied/model unsupported:** verify role, region, and exact target ID. Startup does not contact AWS; the first generation request proves access.
- **Unknown model:** query `GET /v1/models` and use a configured local alias.
- **Codex fallback or stale catalog:** regenerate the Codex configuration and rerun offline `doctor`.
- **Claude model missing:** require `messages_api: true`, canonical `anthropic_model_id`, and function calling; regenerate settings.
- **HTTP 400 for hosted tools:** replace OpenAI-hosted tools with client-side/MCP tools.
- **Port conflict:** select another loopback port and regenerate affected client configuration.

Stop with `Ctrl-C`; the proxy stops admitting requests and gives active requests up to five seconds to drain.
