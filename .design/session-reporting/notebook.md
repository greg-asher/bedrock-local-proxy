# Session Reporting Design Notebook

## Current Position

Add durable, metadata-only session artifacts alongside the existing stdout accounting. Each proxy process creates one private session directory at startup. It writes an initial manifest, appends JSON Lines request events, and writes a final summary when the process exits normally or after controlled shutdown.

## Requested Change

The developer needs a report for every Bedrock Local Proxy session. They need to know where the report lives as soon as the process starts and retain request usage and session totals after the terminal closes.

## Starting Sources

- `cmd/bedrock-proxy/main.go`
- `internal/accounting/accounting.go`
- `internal/accounting/accounting_test.go`
- `internal/config/config.go`
- `docs/planning/bedrock-local-proxy/issues/08-request-accounting.md`
- XDG Base Directory Specification, `XDG_STATE_HOME`: https://cgit.freedesktop.org/xdg/xdg-specs/tree/basedir/basedir-spec.xml

## Relevant Current Behavior

`run` validates configuration, opens the loopback listener, creates one `accounting.Recorder`, attaches it to the server, and writes the final summary through a deferred call. `accounting.Recorder` emits one metadata-only record for each completed request and maintains safe totals. Its only writer is stdout. The current issue explicitly excludes files and persistence.

## Affected Surface

- `cmd/bedrock-proxy`: creates the session artifact before serving and announces its path.
- `internal/accounting`: owns event and summary serialization, totals, and a new file-backed session writer.
- `internal/config`: resolves and validates the report parent directory.
- `config.example.yaml`, `README.md`, and `install.sh`: explain the default location and override.
- Tests: protect permissions, timestamps, atomic finalization, event schema, failure behavior, and the guarantee that prompts, responses, credentials, and headers never reach disk.

## External Research

Question: Where should durable, user-specific session state live?

Finding: the XDG Base Directory Specification defines `XDG_STATE_HOME` for persistent user state and defaults it to `$HOME/.local/state` when unset. This fits session artifacts better than the existing configuration directory. The proxy should resolve `$XDG_STATE_HOME/bedrock-proxy/sessions`, falling back to `~/.local/state/bedrock-proxy/sessions`.

## Candidate Seams and Options

### Redirect stdout to a file

The user could shell-redirect existing output. This has no startup manifest, mixes human and machine formats, does not report the final location, and misses stderr diagnostics. It does not meet the requested per-session reporting experience.

### Mirror existing stdout output to one log file

`io.MultiWriter` could write the present output to a timestamped file. It reuses existing formatting but makes text logs the stored contract, creates no durable start state, and does not offer a stable summary document.

### Add a session artifact writer to accounting

The existing recorder already owns safe request metadata and totals. A session writer can create the directory, write initial state, append JSON Lines events, and finalize a summary without changing HTTP handling. This is the chosen seam.

## Proposed Delta

The proxy creates one directory per execution at startup. Its name is a UTC timestamp plus the process ID and random suffix, preventing collisions. The parent directory is configurable with `reporting.directory`; `--report-dir` overrides it for a single run.

The session directory contains:

- `session.json`: created before binding the listener. It records schema version, session ID, start time, process version, local listen address when available, configured aliases, and lifecycle status. It never records an AWS profile, prompts, responses, request bodies, headers, or credentials.
- `events.jsonl`: append-only JSON Lines. Each line is the current safe startup, request, diagnostic, or summary metadata shape. It is useful for aggregation without parsing text.
- `summary.json`: written atomically after accounting is finalized. It contains the current total fields plus start and end times, final status, and the session ID.

The process prints `Session report: <absolute session directory>` on startup in text mode and emits a `report_path` field on the JSON startup event. An optional per-run `--session-tag` is persisted on every report record and printed at text startup. If it cannot create a required report directory or file, it exits before accepting requests. A listener failure after report creation finalizes `session.json` and `summary.json` with `startup_failed` so the attempted session is still explainable.

All artifact directories use mode `0700` and files mode `0600`. The writer accepts only the existing accounting metadata types. It has no API that accepts request bodies or headers.

## Domain Model

- **Session**: one invocation of `bedrock-proxy`, from report creation through terminal exit.
- **Session directory**: the private filesystem location that owns artifacts for exactly one session.
- **Event**: one append-only metadata record for startup, a completed request, a diagnostic, or finalization.
- **Summary**: the final aggregate for a session. It may be incomplete after forced termination; `session.json.status` communicates that state.

Invariant: a session directory belongs to one process invocation and contains no prompt text, completion text, tool content, request bodies, headers, credentials, or raw upstream errors.

## Transition States

Existing stdout output remains unchanged. File output becomes additional default behavior. `--report-dir` supports a controlled temporary or shared parent directory without changing the configuration file.

## Decisions

- Store reports by default. The request is for reporting for each proxy session, so optional capture would make the common path unreliable.
- Use JSON Lines for events and JSON for summary. The formats are stable, easy to inspect, and do not depend on the chosen stdout log format.
- Create state before listener binding. This records port conflicts and other startup failures instead of losing them.
- Fail closed when required report creation fails. Silent loss of a requested report is worse than a visible startup error.
- Omit AWS profile from persistent artifacts. It is not required for usage analysis and the repository recently removed a real profile name from public material.

## Research and Prototypes

No prototype is needed. The existing recorder serializes the complete metadata set required for request and summary artifacts.

## Active Change Frontier

Recognized proxy session directories older than 30 days are removed on startup, including incomplete sessions. Cleanup skips links and unknown directories; failures produce a safe warning and do not stop a new session. The report directory, contents, lifecycle, permissions, and failure behavior are settled.

## Decision Map

- Status: not needed
- Path: none
- Destination: none
- Return condition: none

## Best Next Move

Implement the file-backed session writer at the accounting boundary, then add startup path reporting and tests for privacy, finalization, and report-directory failure.
