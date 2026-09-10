# Complete the live Pi acceptance scenario

## Useful outcome

A developer uses Pi for a real coding task with only a local API URL, dummy key, and friendly model.

## What changes

Use `./install.sh` to build and install the current source before the live check, following issue 11’s local setup instructions.

Document the tested Pi version and its actual provider/model configuration. Run against the developer-configured model using profile Halo-Win-Agent-Execution in us-east-2. The developer performs AWS SSO login and updates model IDs; do not provision IAM or change their model choices.

In a disposable repository, have Pi inspect a file, perform an edit using tools, and verify the result. Observe streamed output, metadata-only logs, usage when upstream provides it, and estimated cost when configured. Cancel a second generation and verify proxy-side cancellation, then exit and reconcile the session summary.

Exercise each OpenAI endpoint against a configured target where supported. If Responses is unsupported by available targets, record the actual upstream rejection and retain its synthetic positive-path evidence; do not claim live Responses success. Document the local setup and authentication troubleshooting.

## Requirements and delivery context

Authority: [Product Brief](../bedrock-local-proxy-product-brief.docx), plus the user’s clarifications: use actual profile `Halo-Win-Agent-Execution` and region `us-east-2`; the user supplies model IDs in YAML; Pi is the primary acceptance client; add Anthropic Messages for Claude Code. Keep profile, region, targets, and prices configurable. No exact model ID is required for implementation tests. Never log prompts, responses, tool content, credentials, or raw sensitive headers. The product remains a localhost-only Go executable with no hosted infrastructure.

Brief section 22; user selects Pi, not OpenCode, for full acceptance. This is the live integration gate. Missing AWS session, client installation, or usable model configuration blocks this issue, not earlier implementation. Credential refresh/expiry fault behavior is established by controlled tests; report that separately from live SSO success.

## Done when

- Pi completes a live tool-based edit through localhost and the resulting file change is verified.
- Evidence records client version, protocol, configured model used, streaming, cancellation, request accounting, and shutdown results without storing private payloads.
- Setup instructions use the actual Pi configuration mechanism tested, not assumed environment-variable support.
- Live success, unsupported endpoint results, and simulated credential lifecycle tests are clearly distinguished.
- No access keys are exported/copied for the workflow; expired-session troubleshooting shows the configured-profile SSO command.

## Depends on

- [11: Build and install locally with one command](11-native-binaries.md)
- [08: Report request usage and a truthful session summary](08-request-accounting.md)
