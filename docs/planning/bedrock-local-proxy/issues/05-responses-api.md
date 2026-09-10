# Serve normal and streaming Responses requests

## Useful outcome

Responses API callers can use locally named models on supported Bedrock targets.

## What changes

Add `POST /v1/responses` using the signed transport and established relay lifecycle. Verify this endpoint’s current native route and request/stream contracts rather than assuming Chat Completions shapes. Preserve Responses input items, tool calls/results, unknown fields, and upstream errors. Apply configuration defaults only through documented Responses fields; preserve explicit endpoint-specific values.

Implement normal and streamed usage observers specific to Responses, including presence of uncovered billing dimensions. Use the shared completion result for every normal and streaming exit; observe normal bodies with bounded memory too. Reuse the bounded relay; do not build another streaming engine. Recognize successful and failed terminal events, and classify incomplete streams. Unsupported model/endpoint combinations retain their upstream failure; do not emulate Responses through Chat Completions.

Record contract fixtures and supported behavior in tests. Live support for the user-configured targets is recorded during acceptance; lack of a model ID does not block implementation.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief section 7 requires Responses where supported upstream. Issues 03–04 establish local error behavior, preservation rules, and the shared completion lifecycle. This issue owns Responses field/default mapping and usage extraction; no response storage, retrieval, conversation database, or additional endpoint is included. Synthetic fixtures must use the real Responses request, response item, tool, error, usage, and streaming event shapes for the supported upstream contract.

## Done when

- Normal and streamed synthetic Responses exchanges preserve input items, tool exchanges, response bytes, event order, and explicit request values.
- Tests demonstrate the configured max_tokens default maps only to the documented Responses token-limit field and never overrides its explicit client value.
- Success/failure terminal events and missing/repeated usage are accounted for using Responses semantics.
- Unsupported upstream combinations return their actual error; cancellation and interrupted streams reuse the shared behavior.
- Any native-route incompatibility requiring architectural change is reported explicitly rather than hidden by translation.

## Depends on

- [04: Stream chat output and cancel abandoned generation](04-chat-streaming.md)
