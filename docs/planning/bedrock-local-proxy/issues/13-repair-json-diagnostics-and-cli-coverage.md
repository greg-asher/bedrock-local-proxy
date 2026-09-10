# Repair JSON diagnostics and complete local CLI coverage

## Useful outcome

The startup command has one consistent machine-readable error contract and local tests prove every configuration and shutdown path without AWS access.

## What changes

Parse flags in a way that preserves the requested log format for parse errors. When `--log-format json` is present, emit one valid JSON diagnostic for an unknown flag or invalid flag value; otherwise retain human-readable flag errors. Avoid duplicate flag usage text in JSON mode.

Extend command-level tests to cover default home-based configuration, explicit `--config`, startup text, startup JSON, invalid and non-loopback configuration, occupied ports, `--version` without config, SIGINT, and SIGTERM. Use temporary files, ports, and subprocesses. Tests must not require AWS credentials, model access, network access, or the user’s real home configuration.

Use the real service and client wire shapes in every fake: AWS credential and SigV4 headers, OpenAI model-list and completion envelopes, Anthropic Messages headers and SSE events, and the client startup/configuration contracts. Do not replace a real contract with an invented test-only schema.

## Requirements and delivery context

This closure repair follows issue 12’s review. The affected flow is flag input → format selection → diagnostic output, plus command-level proof of configuration selection and lifecycle behavior. Preserve the localhost-only boundary, exact default path, user-editable configuration, actual profile/region example, and no-secret/no-content logging.

## Done when

- Unknown and invalid flags produce one valid JSON object when JSON mode is requested and readable text otherwise.
- Command tests cover every listed path and pass with no AWS environment or network.
- Fakes used by the local suite match the real AWS, OpenAI, Anthropic, Pi, and Claude Code contracts they stand in for.
- Existing config, server, and build checks continue to pass.

