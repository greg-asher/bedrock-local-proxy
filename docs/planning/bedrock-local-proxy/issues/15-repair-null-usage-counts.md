# Treat null usage counts as unknown

## Useful outcome

Request accounting never reports a fabricated zero when an upstream protocol sends a JSON `null` token count.

## What changes

Harden the shared OpenAI streaming and Anthropic Messages usage observers so `null`, malformed, or absent token counts remain unknown. Preserve valid zero counts. Add contract fixtures for null input and output counts in both normal Messages responses and streaming Chat Completions usage events; terminal stream detection and response delivery must remain unchanged.

## Requirements and delivery context

This repair follows adversarial review of issue 04 and closes the same decoding edge case in the already-completed issue 06 parser. The real OpenAI and Anthropic usage fields are integer-or-null at the JSON boundary for this proxy’s purposes. No AWS credentials or network are needed.

## Done when

- A streaming usage event with `null` input or output counts completes normally but records those totals as unknown rather than zero.
- A normal Anthropic Messages response with a `null` count preserves the response and leaves that total unknown.
- Valid numeric zero remains a known zero.
- Existing local tests, race tests, vet, build, and diff checks pass.

## Depends on

- [03: Complete a non-streaming chat request](03-chat-completions.md)
- [04: Stream chat output and cancel abandoned generation](04-chat-streaming.md)
- [06: Serve a non-streaming Anthropic Messages request](06-messages-api.md)
