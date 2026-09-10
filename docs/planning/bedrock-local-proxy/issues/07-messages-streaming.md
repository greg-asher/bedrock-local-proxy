# Stream Anthropic Messages with tool use

## Useful outcome

An Anthropic caller receives incremental content and tool arguments with correct cancellation and completion.

## What changes

Enable stream=true for Messages using the shared relay from issue 04. Preserve Anthropic SSE event names, data, pings, comments, and order. Do not convert the stream into OpenAI chunks.

Implement the Anthropic usage observer: account for initial and subsequent usage according to documented semantics without adding cumulative totals twice. Preserve unknown/cache-related usage dimensions as evidence for estimate completeness. Recognize the Messages terminal event and streamed error events. Cancellation, oversized observer records, shutdown, and post-header errors follow the shared relay contract.

Integrate both normal and streaming Messages outcomes with the shared completion result so issue 08 consumes one protocol-independent contract. Use synthetic text and tool-call streams to verify behavior. Client setup and live Claude Code validation belong to issue 10.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

User addition plus brief sections 10–14. Issue 06 owns non-streaming Messages and headers; issue 04 owns the reusable relay. This dependency makes shared-code ownership explicit and avoids two agents inventing incompatible stream lifecycles. No token-counting endpoint or full Claude Code feature parity is implied.

## Done when

- Text and tool argument fragments arrive before upstream completion with SSE bytes and pings preserved.
- Tests cover message-start usage, later cumulative usage, message-stop, error events, split records, and premature EOF.
- Client disconnect cancels the upstream request; every exit produces one completion outcome.
- Unknown or oversized usage cannot break streaming or become fabricated token totals.
- Protocol tests require no real model IDs or installed Claude Code.

## Depends on

- [04: Stream chat output and cancel abandoned generation](04-chat-streaming.md)
- [06: Serve a non-streaming Anthropic Messages request](06-messages-api.md)
