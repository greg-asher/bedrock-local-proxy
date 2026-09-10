# Report request usage and a truthful session summary

## Useful outcome

The developer sees metadata-only request records and process totals across all delivered protocols.

## What changes

Consume the optional usage totals and completion outcomes produced by endpoint issues. Do not add protocol parsing here. Emit one human-readable or JSON request record with timestamp, endpoint, local/upstream model, latency, HTTP status when available, input/output tokens when known, and estimated cost. Cover local rejections as well as forwarded requests; missing model/status data remains unavailable.

For generation requests, count requests, successes, and failures exactly once. Cancellation and interrupted streams are failures even after HTTP 200. Model-list requests can be logged but do not contribute to generation/token/cost totals. Keep updates concurrency-safe.

Calculate estimates only when required usage and both input/output prices are known: input / 1e6 × input_price + output / 1e6 × output_price. Zero is valid; missing is unknown. If observed billing dimensions are not covered by configured prices, report cost=n/a rather than a complete-looking estimate. Extend pricing only for a demonstrated model need, not speculatively.

On graceful shutdown, finalize request accounting after drain/cancellation and print runtime, requests, successes, failures, known token totals, and known estimated cost. Disclose how many requests lack usage or estimates. Restart resets everything. JSON mode emits valid records for the summary too.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief sections 12–15. Protocol issues own extraction; this issue owns aggregation and presentation. Prices are estimates supplied by the user, never fetched automatically. Unknown totals cannot be presented as zero or a complete session bill. Logs go only to stdout/stderr; no files, rotation, persistence, or remote telemetry.

## Done when

- A mixed set of normal, streamed, rejected, and canceled generation requests yields requests = successes + failures with no duplicates.
- Known usage/prices yield the formula result; missing prices, missing usage, zero prices, and uncovered billing dimensions have tested distinct outcomes.
- Concurrent completion and shutdown produce consistent totals and disclose incomplete coverage.
- Every JSON output line parses; human output labels cost as estimated and unknown values as unavailable.
- Sentinel content and secrets in request bodies, headers, and upstream errors never appear in emitted logs.

## Depends on

- [05: Serve normal and streaming Responses requests](05-responses-api.md)
- [07: Stream Anthropic Messages with tool use](07-messages-streaming.md)
