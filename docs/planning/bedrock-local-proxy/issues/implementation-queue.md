# Bedrock Local Proxy Implementation Queue

Each issue appears in exactly one state. Ready means no unresolved issue prerequisite. Move an issue to Ready only when its listed prerequisites are Done. Implemented means code is present but review or required verification remains; Done means its completion criteria passed.

Delivery basis: [Product Brief](../bedrock-local-proxy-product-brief.docx), unchanged, plus the user’s clarifications: actual profile `Halo-Win-Agent-Execution`, region `us-east-2`, user-maintained model IDs, Pi acceptance, and Anthropic Messages for Claude Code. The workspace has planning files only. No implementation or live AWS/client validation has been performed.

Local implementation and deterministic contract tests must not require AWS credentials, model access, or network access. Issues 01–08 and 11 can be built and reviewed locally with fake credentials/providers and fake upstreams, but those fakes must use the real AWS, OpenAI, Anthropic, Pi, and Claude Code wire shapes. Issues 09 and 10 contain live-client gates that remain externally blocked until this repository is moved to the AWS-enabled test machine; that external gate does not block local implementation.

The previous six assignments are replaced by these eleven. Endpoint contract verification now belongs to each endpoint issue; there is no all-protocol investigation gate. Protocol issues own their usage parsers, issue 08 owns accounting, and issues 09–10 own live acceptance. Model IDs are not a prerequisite to implementation. Missing credentials or usable configuration can block live acceptance only.

Issue 01 is the initial Ready assignment. Issue 02 follows once the Go module and configuration exist. The first normal OpenAI and Anthropic routes follow independently. Issue 04 establishes the shared streaming lifecycle reused by Responses and Messages. Packaging can begin after local startup and does not wait for AWS access.

V1 completion requires every issue Done, both live client gates passed, and the local binary installed from the accepted source. Developers build locally with `./install.sh`; no download installer or external publication is included.

## Ready

- [06: Serve a non-streaming Anthropic Messages request](06-messages-api.md)
- [14: Repair the OpenAI tool-call contract fixture](14-repair-openai-tool-fixture.md)

## In progress

None.

## Blocked

- [04: Stream chat output and cancel abandoned generation](04-chat-streaming.md) — blocked by [03](03-chat-completions.md).
- [05: Serve normal and streaming Responses requests](05-responses-api.md) — blocked by [04](04-chat-streaming.md).
- [07: Stream Anthropic Messages with tool use](07-messages-streaming.md) — blocked by [04](04-chat-streaming.md), [06](06-messages-api.md).
- [08: Report request usage and a truthful session summary](08-request-accounting.md) — blocked by [05](05-responses-api.md), [07](07-messages-streaming.md).
- [09: Complete the live Pi acceptance scenario](09-pi-acceptance.md) — blocked by [08](08-request-accounting.md), [11](11-native-binaries.md), and the AWS-enabled test machine.
- [10: Verify Claude Code through the Messages endpoint](10-claude-code-acceptance.md) — blocked by [08](08-request-accounting.md), [11](11-native-binaries.md), and the AWS-enabled test machine.

## Implemented

- [03: Complete a non-streaming chat request](03-chat-completions.md)
- [11: Build and install locally with one command](11-native-binaries.md)

## Done

- [01: Start locally and list configured models](01-local-startup.md)
- [02: Send signed requests using the developer’s AWS credentials](02-signed-aws-transport.md)
- [12: Repair local startup evidence and endpoint reporting](12-repair-local-startup.md)
- [13: Repair JSON diagnostics and complete local CLI coverage](13-repair-json-diagnostics-and-cli-coverage.md)
