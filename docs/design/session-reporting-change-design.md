# Session Reporting Change Design

## Executive Summary

Bedrock Local Proxy should create a private report directory for every execution. The directory is created before the server listens, announced at startup, and receives safe request events plus a final usage summary. The change extends `internal/accounting`, which already owns the only metadata the proxy is allowed to retain.

## Requested Outcome

A developer starts the proxy and immediately sees the report location. After the session, they can inspect request outcomes, token usage, estimated cost, and session totals without keeping the terminal open.

The report must never store prompts, responses, tool content, request bodies, headers, AWS credentials, or raw upstream errors.

## Relevant Current Behavior

`cmd/bedrock-proxy/main.go` creates one accounting recorder after it binds the listener. The recorder writes startup information, request records, and the final summary to stdout. `internal/accounting/accounting.go` already records the required safe fields: endpoint, local and upstream model, latency, HTTP status, outcome, token counts, pricing status, and estimated cost.

The current plan at `docs/planning/bedrock-local-proxy/issues/08-request-accounting.md` intentionally limits output to stdout and stderr. Durable session reports change that boundary and must replace the no-file claim in the public documentation.

## Affected Surface

The CLI must create and announce a session directory. The accounting package must serialize events and summaries to files while keeping current stdout behavior. The configuration package must resolve a report parent directory, and configuration documentation must explain the default and `--report-dir` override.

The HTTP server and Bedrock transport do not change. They continue to publish only `CompletionResult` metadata to accounting.

## External Finding That Shaped the Design

The XDG Base Directory Specification reserves `XDG_STATE_HOME` for persistent user state and defaults it to `$HOME/.local/state` when unset. Session reports are persistent application state, so the default parent directory should be `$XDG_STATE_HOME/bedrock-proxy/sessions`, or `~/.local/state/bedrock-proxy/sessions` when the variable is empty. [XDG Base Directory Specification](https://cgit.freedesktop.org/xdg/xdg-specs/tree/basedir/basedir-spec.xml)

## Options and Candidate Seams

Shell redirection works for an ad hoc capture but cannot create an early report marker, stable JSON records, or a final report location. Mirroring stdout to a file improves persistence but ties the stored contract to display formatting.

The selected option adds a session artifact writer behind `internal/accounting.Recorder`. It reuses the only safe request and total data already produced by the application. The server still sends no prompt or response content to accounting.

## Proposed Delta

Each run creates one private session directory. Its name combines the UTC start timestamp, process ID, and random suffix. This prevents conflicting writers and makes folder order meaningful.

The parent directory is selected in this order:

1. `--report-dir PATH` for one run.
2. `reporting.directory` in the YAML configuration.
3. `$XDG_STATE_HOME/bedrock-proxy/sessions`.
4. `~/.local/state/bedrock-proxy/sessions`.

Each session directory contains:

| File | Written | Contents |
| --- | --- | --- |
| `session.json` | startup and finalization | Schema version, session ID, timestamps, status, binary version, listen address, aliases, and report path. |
| `events.jsonl` | throughout the session | Append-only startup, request, diagnostic, and summary metadata. |
| `summary.json` | finalization | Request totals, known token totals, estimated cost, missing data counts, incomplete request count, and final status. |

`summary.json` is written to a temporary file and renamed into place. A crash can leave an initial `session.json` and partial `events.jsonl`; `status: running` makes the missing finalization explicit. A listener or other startup failure finalizes the session with `status: startup_failed`.

The report location appears in text startup output and as `report_path` in the JSON startup record. An optional `--session-tag` is validated once per process, printed in text mode, and carried through every report record. The proxy fails before accepting requests if it cannot create the required directory or files.

Directories use mode `0700`; report files use mode `0600`. Persistent artifacts omit the AWS profile because it is not needed for usage reporting.

## Decisions

Reports are on by default because the requested behavior is one report per session. The current stdout output remains for interactive use.

The persistent format is always structured JSON, independent of `--log-format`. This makes report parsing stable and avoids mixing human display text with data.

The report is initialized before listener binding. This records startup failures such as a busy port, which are part of the session’s useful evidence.

## Remaining Design Questions

On startup, the proxy removes direct-child session directories with a valid proxy manifest older than 30 days, including incomplete sessions. It never traverses symlinks or removes unknown directories. A cleanup failure records a safe warning and startup continues.
