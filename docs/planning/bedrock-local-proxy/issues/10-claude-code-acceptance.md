# Verify Claude Code through the Messages endpoint

## Useful outcome

A developer can configure Claude Code to complete a small coding task through the local proxy.

## What changes

Use `./install.sh` to build and install the current source before the live check, following issue 11’s local setup instructions.

Document and test the selected Claude Code version using the localhost Anthropic base URL, explicit dummy credential, and a developer-configured Claude model. Include auxiliary model selections needed by the tested workflow so they do not become unknown local names.

Run a small tool-call/tool-result edit in a disposable repository. Verify streaming, cancellation, safe metadata logging, and usage accounting. Confirm operation with the existing OpenAI model listing; use explicit client model selection where sufficient. Do not silently alter GET /v1/models for another protocol.

Observe optional startup/token-counting calls. Record their nonfatal behavior when optional. If the acceptance workflow requires an additional endpoint or incompatible model discovery, record the minimal concrete requirement and update the issue graph before adding scope.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

User requested Claude CLI support; the plan interprets this as Claude Code, with a focused compatibility gate distinct from Pi. [Claude Code protocol reference](https://code.claude.com/docs/en/llm-gateway-protocol) guides the checks. No OpenCode requirement, full-feature parity, automatic fallback, or hosted gateway work is included. Local Claude Code fixtures and configuration checks must use Claude Code’s actual Anthropic base URL, headers, request bodies, and SSE shapes; do not substitute a bespoke test client.

Complete all local implementation and synthetic contract checks without AWS credentials. This issue remains externally blocked only for the live Claude Code run until the accepted source is available on the AWS-enabled test machine.

## Done when

- Claude Code completes a live tool-based edit via /v1/messages using a friendly Claude model and existing AWS identity.
- Tested setup states the exact client version, base URL, dummy credential mechanism, and required model selections.
- Streaming and cancellation work; logs and session totals reflect the request outcomes without content.
- Optional endpoint failures and unsupported features are documented accurately; no necessary compatibility gap is declared passed.
- Lack of a usable configuration or AWS session leaves this issue blocked, without reopening model selection in implementation issues.

## Depends on

- [11: Build and install locally with one command](11-native-binaries.md)
- [08: Report request usage and a truthful session summary](08-request-accounting.md)
