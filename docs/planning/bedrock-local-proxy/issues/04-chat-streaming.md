# Stream chat output and cancel abandoned generation

## Useful outcome

A caller receives Chat Completions incrementally and can stop upstream work by disconnecting.

## What changes

Extend Chat Completions to stream=true. Relay response chunks and flush without waiting for completion. Preserve SSE bytes, comments, keep-alives, and terminal markers. Carry client cancellation upstream and close resources when either side ends.

Observe usage as a side channel without rewriting the stream or forcing usage-related request options. Bound observer memory independently of total response size. If a record exceeds the observer’s limit or cannot be parsed, continue forwarding and mark usage unavailable; do not retain the whole response. Distinguish cumulative usage from additive events using this protocol’s documented semantics. An oversized or malformed usage record must not disable recognition of later terminal/error events. Keep completion detection independent from usage availability; retain no unbounded event payload.

Report completion once: normal terminal event, upstream rejection, cancellation, or interrupted stream. EOF before the expected terminal marker is interrupted, even after HTTP 200. After headers are sent, never append a local JSON error. On shutdown, stop admission, allow a documented bounded drain, then cancel remaining upstream work. Choose and document the drain duration as an implementation setting, without adding another CLI configuration system.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `YOUR_AWS_PROFILE` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief sections 10–12 and 22. This issue extends issue 03’s completion result and owns the reusable byte-relay and streaming completion lifecycle used by later protocols, plus the Chat Completions stream observer. Cancellation proves that the proxy stops its upstream request; do not claim it proves provider billing stopped. Streaming fixtures must use real OpenAI SSE event/data shapes, usage placement, keep-alives, and terminal markers.

## Done when

- A controlled upstream withholds completion until the test sees the first client chunk, proving incremental forwarding without timing guesses.
- A stream larger than the observer limit forwards intact while observer memory stays bounded; split and multiple SSE records are handled.
- Disconnect and shutdown tests observe upstream context cancellation and closed resources.
- Usage-only final chunks, repeated totals, no usage, malformed observer data, and EOF before the terminal marker produce explicit results.
- An upstream failure after headers yields one failed completion and no injected JSON body.

## Depends on

- [03: Complete a non-streaming chat request](03-chat-completions.md)
