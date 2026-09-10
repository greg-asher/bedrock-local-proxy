# Send signed requests using the developer’s AWS credentials

## Useful outcome

All endpoints can use one tested AWS transport without copying keys or implementing their own refresh logic.

## What changes

Build a reusable Go transport using the official AWS SDK credential provider chain, configured profile and region, and SigV4 signing. Retrieve credentials immediately before signing the final request method, URL, headers, and body. Let the SDK own credential caching and refresh.

The transport accepts a request and context and returns the upstream HTTP response or a classified transport failure. It does not resolve models, translate payloads, parse streams, or write client error envelopes. Use a fake credential provider and local HTTP transport to test signing and lifecycle behavior without AWS access.

Strip incoming Authorization, x-api-key, and AWS security/signature headers before generating upstream authentication. Remove hop-by-hop headers, including those named by Connection. Fix the upstream authority from the configured AWS region; do not follow redirects to another destination or accept a destination from client input. Preserve cancellation through signing and HTTP execution. Set the upstream Host and recompute body length after request transformation; never forward a stale Content-Length. Remove hop-by-hop response headers before returning them downstream. Do not automatically retry generation requests or replay a partial stream. Set bounded connection/header waits without imposing a short whole-response deadline on active streams; document the chosen transport limits and test stalled connection/header behavior.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief sections 8–11 and 19. This enabling component is shared by all three generation protocols. Issue 01 establishes the Go module and configuration types before this component is implemented. It does not depend on any generation handler. Keep test substitution internal, not a user-facing remote upstream feature. Classify expired SSO, unavailable credentials, signing failure, and connectivity failure. An upstream 403 alone is not proof of expired SSO.

## Done when

- A deterministic signing test verifies service, region, payload hash, and temporary-session credentials on the outgoing request.
- A fake expiring provider supplies replacement credentials for a later call without a process restart; no separate raw-key cache is introduced.
- Cancellation reaches a waiting upstream request; client credentials and hop-by-hop headers are absent from the signed request.
- Redirect and client-controlled destination tests cannot send credentials outside the configured upstream.
- Failures expose safe classifications to handlers without logging keys, headers, or bodies. No live model ID is required.

## Depends on

- [01: Start locally and list configured models](01-local-startup.md)
