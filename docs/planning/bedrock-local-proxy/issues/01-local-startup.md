# Start locally and list configured models

## Useful outcome

A developer can launch the Go binary and inspect available friendly model names without contacting AWS.

## What changes

Create the Go module, executable, YAML loader, and local HTTP server. Support the default config path `~/.config/bedrock-proxy/config.yaml`, `--config`, `--log-format json`, and `--version`. Default listen address: `127.0.0.1:8787`. Report profile, region, listening URL, and model names at startup.

Load version 1 configuration with AWS profile/region and model entries containing `bedrock_model_id`, optional `display_name`, optional input/output prices per million, and optional `temperature`/`max_tokens` defaults. Reject missing required values, duplicate YAML keys, invalid types, unsupported versions, and negative prices. Allow multiple public names to share a target. Reject wildcard and non-loopback listen addresses. Implement configuration-derived OpenAI `GET /v1/models` with public names as IDs. Unknown routes return 404 and unsupported methods on known routes return 405 without AWS calls. The default config path is resolved from the user home directory regardless of working directory; an explicit relative --config path is relative to the invocation directory.

Provide a minimal example with the actual AWS profile and region and clearly replaceable model IDs. Stop accepting connections on SIGINT/SIGTERM; later stream and accounting issues extend shutdown behavior. Do not implement generation, AWS calls, or per-request default merging here.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `YOUR_AWS_PROFILE` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief sections 5–6, 16–19. This issue owns initial module/build setup and server configuration. Keep the package structure small. Version output must not require config or credentials. JSON mode applies to startup and diagnostics as well as later request records.

## Done when

- Default and explicit config paths work; missing/invalid files exit nonzero with a useful diagnostic containing no config dump.
- Model listing returns configured public IDs without an AWS call; duplicate targets remain separate public entries.
- Tests cover invalid configuration, loopback binding, occupied ports, and version output without config.
- Startup output is readable in default mode and each emitted record is valid JSON in JSON mode.
- A signal stops the server; installation and startup instructions explain the user-editable configuration.
