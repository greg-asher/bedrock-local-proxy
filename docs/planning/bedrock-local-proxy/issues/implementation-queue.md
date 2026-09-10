# Bedrock Local Proxy Implementation Queue

Each issue appears in exactly one state. Ready means no unresolved issue prerequisite. Move an issue to Ready only when its listed prerequisites are Done. Implemented means code is present but review or required verification remains; Done means its completion criteria passed.

Delivery basis: [Product Brief](../bedrock-local-proxy-product-brief.docx), unchanged, plus the user’s clarifications: actual profile `Halo-Win-Agent-Execution`, region `us-east-2`, user-maintained model IDs, Pi acceptance, and Anthropic Messages for Claude Code. The workspace has planning files only. No implementation or live AWS/client validation has been performed.

The previous six assignments are replaced by these eleven. Endpoint contract verification now belongs to each endpoint issue; there is no all-protocol investigation gate. Protocol issues own their usage parsers, issue 08 owns accounting, and issues 09–10 own live acceptance. Model IDs are not a prerequisite to implementation. Missing credentials or usable configuration can block live acceptance only.

Issue 01 is the initial Ready assignment. Issue 02 follows once the Go module and configuration exist. The first normal OpenAI and Anthropic routes follow independently. Issue 04 establishes the shared streaming lifecycle reused by Responses and Messages. Packaging can begin after local startup and does not wait for AWS access.

V1 completion requires every issue Done, both live client gates passed, and the local binary installed from the accepted source. Developers build locally with `./install.sh`; no download installer or external publication is included.

## Ready

- [02: Send signed requests using the developer’s AWS credentials](02-signed-aws-transport.md)

## In progress

None.

## Blocked

- [11: Build and install locally with one command](11-native-binaries.md) — blocked by [01](01-local-startup.md).
- [03: Complete a non-streaming chat request](03-chat-completions.md) — blocked by [01](01-local-startup.md), [02](02-signed-aws-transport.md).
- [04: Stream chat output and cancel abandoned generation](04-chat-streaming.md) — blocked by [03](03-chat-completions.md).
- [05: Serve normal and streaming Responses requests](05-responses-api.md) — blocked by [04](04-chat-streaming.md).
- [06: Serve a non-streaming Anthropic Messages request](06-messages-api.md) — blocked by [01](01-local-startup.md), [02](02-signed-aws-transport.md).
- [07: Stream Anthropic Messages with tool use](07-messages-streaming.md) — blocked by [04](04-chat-streaming.md), [06](06-messages-api.md).
- [08: Report request usage and a truthful session summary](08-request-accounting.md) — blocked by [05](05-responses-api.md), [07](07-messages-streaming.md).
- [09: Complete the live Pi acceptance scenario](09-pi-acceptance.md) — blocked by [08](08-request-accounting.md), [11](11-native-binaries.md).
- [10: Verify Claude Code through the Messages endpoint](10-claude-code-acceptance.md) — blocked by [08](08-request-accounting.md), [11](11-native-binaries.md).

## Implemented

- [01: Start locally and list configured models](01-local-startup.md) — awaiting repair [12](12-repair-local-startup.md) before review can mark it Done.
- [12: Repair local startup evidence and endpoint reporting](12-repair-local-startup.md)

## Done

None.
