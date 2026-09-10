# Serve a non-streaming Anthropic Messages request

## Useful outcome

An Anthropic-compatible caller gets a Messages response through a configured Claude target.

## What changes

Add non-streaming `POST /v1/messages` through the native Bedrock Anthropic route. Verify the route’s SigV4 and header contract against current AWS documentation; isolate any incompatibility instead of introducing InvokeModel conversion or another authentication system without a decision.

Require a JSON object and a nonempty string model. Reject malformed requests and unknown local model names before calling AWS; list configured alternatives for an unknown name. Resolve model names from issue 01’s configuration and apply defaults only to absent fields. Explicit null and zero remain client values. Keep this handler independent of the OpenAI handler; shared helpers may be extracted when both implementations exist. Preserve system block structure, tool blocks/results, thinking fields, cache controls, unknown fields, and exact numeric values. Forward anthropic-version and anthropic-beta on the native route. Route matching must accept a query string such as ?beta=true. Client authentication is replaced by the shared AWS transport.

Return upstream bodies/statuses unchanged. Local failures use Anthropic-shaped error envelopes: malformed request or unknown model is 400, unavailable/expired AWS credentials is 401, and signing/connectivity failure is 502. Expired SSO includes the configured-profile login command. Preserve upstream rejection statuses. Do not depend on OpenAI response encoding. Extract optional input/output usage and presence of uncovered billing dimensions from normal Messages responses using bounded observation memory. Missing or malformed usage remains unknown without altering response delivery. Record one completion outcome for normal delivery, upstream/local rejection, cancellation, and failed body copy. Issue 07 integrates these outcomes into the shared completion result established by issues 03–04. Streaming is delivered in issue 07; reject stream=true explicitly until then.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

User addition to the brief: Anthropic Messages for Claude Code. This is a separate protocol, not a requirement to translate OpenAI into Anthropic. Depends only on local configuration and AWS transport. References: [AWS Messages](https://docs.aws.amazon.com/bedrock/latest/userguide/inference-messages-api.html), [Claude Code protocol](https://code.claude.com/docs/en/llm-gateway-protocol). Token counting and Anthropic model discovery are not added here.

## Done when

- A localhost Messages exchange resolves the local Claude name and preserves system/tool content and version/beta headers.
- Tests cover query strings, explicit defaults, malformed requests, unknown models, expired credentials, and upstream errors using Anthropic envelopes where locally generated.
- Normal usage is extracted without changing response bytes; missing usage remains unknown.
- No credentials, system prompts, tool payloads, or raw error bodies enter logs.
- Native-route contract evidence is recorded; a required architecture change remains explicit.

## Depends on

- [01: Start locally and list configured models](01-local-startup.md)
- [02: Send signed requests using the developer’s AWS credentials](02-signed-aws-transport.md)
