# Complete a non-streaming chat request

## Useful outcome

An OpenAI caller gets a complete Chat Completions response through a friendly local model.

## What changes

Implement non-streaming `POST /v1/chat/completions` using the shared transport and configuration. Verify the native Bedrock route and signing requirements against current AWS documentation and encode representative synthetic contract tests. User model IDs remain configuration values.

Require a JSON object with a nonempty string model. Unknown local names fail before AWS and list configured alternatives. Change only the model value and absent configured defaults; preserve unknown fields and explicit values, including null and zero. Preserve JSON number precision when modifying payloads. Use the endpoint’s documented field names; do not add unsupported defaults or silently drop client parameters.

Preserve upstream response status, body, and end-to-end response headers. Generate OpenAI-shaped errors for local validation/transport failures: malformed request or unknown model is 400; unavailable/expired AWS credentials is 401; signing or connectivity failure is 502. Expired SSO includes the configured-profile login command. Preserve upstream errors rather than reclassifying every 403.

Establish the internal completion result consumed by accounting: endpoint, optional local/upstream model, elapsed time, optional HTTP status, success/failure/cancellation outcome, optional input/output totals, and whether observed usage includes uncovered pricing dimensions. Finalize it once for every handler exit, including local rejection and downstream write failure. A normal response succeeds only after a successful upstream status and complete delivery; a failed body copy is a failure. Extract normal-response token usage into optional input/output totals for later accounting. Missing or malformed usage stays unknown and cannot alter the response sent to the caller. Capture the presence of additional billing dimensions as well as basic totals; do not retain arbitrary response content. Bound usage-observation memory for normal responses too; exceeding that limit disables observation without truncating the forwarded body. Streaming is delivered in issue 04; until then, reject stream=true explicitly before forwarding.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief sections 6–11, 14, 22. Depends on the configuration server and signed transport. Use semantic JSON preservation rather than promising byte-identical request bodies after model rewriting. This issue owns normal Chat Completions usage extraction. No Pi installation or live AWS access is needed for automated completion.

## Done when

- A localhost request to a fake upstream receives its normal completion, with friendly model resolution and correct defaults demonstrated.
- Tests preserve tool calls/results, unknown fields, explicit null/zero values, and large numeric values across request transformation.
- Malformed JSON, missing model, unknown model, expired credentials, network failures, and upstream rejections follow the stated error contract.
- A response with usage exposes correct optional totals; missing usage is not zero; response bytes remain unchanged.
- Synthetic sentinel prompts, responses, and credentials never appear in diagnostics.

## Depends on

- [01: Start locally and list configured models](01-local-startup.md)
- [02: Send signed requests using the developer’s AWS credentials](02-signed-aws-transport.md)
