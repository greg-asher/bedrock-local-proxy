# Repair the OpenAI tool-call contract fixture

## Useful outcome

The Chat Completions local contract test proves tool-call preservation with a request shape accepted by real OpenAI-compatible clients.

## What changes

Use the valid message sequence `user` → `assistant` with `tool_calls` → `tool` with the matching `tool_call_id` in the large-payload preservation fixture. Keep the large numeric argument and unknown-field assertions so the test still covers the original behavior.

## Requirements and delivery context

This repair follows the initial review of issue 03. The test fixture must match the real Chat Completions request envelope; it must not attach assistant-only `tool_calls` to a user message. No AWS credentials or network are needed.

## Done when

- The fixture contains a valid user/assistant/tool exchange and the existing test passes unchanged against the transformed request.
- The test still proves tool payload, matching tool-call ID, unknown fields, explicit null/zero values, and large-number precision survive transformation.

