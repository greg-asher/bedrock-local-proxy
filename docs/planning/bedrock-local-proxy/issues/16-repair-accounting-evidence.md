# Repair accounting metadata and shutdown ordering

## Useful outcome

Every metadata-only request record is useful in text and JSON mode, and the final summary includes all completions after graceful cancellation.

## What changes

Include timestamp, endpoint, local and upstream model names, latency, known token fields, HTTP status, outcome, usage status, and cost status in human-readable request records as well as JSON. Propagate the status actually written for local validation, transport, and shutdown-admission failures into `CompletionResult`, including model-list encoding failures. Treat every unknown Chat usage dimension as uncovered billing data. During shutdown, cancel active requests and wait for their completion callbacks before emitting the summary, while retaining the bounded drain contract. Exercise the wiring with mixed real-shaped endpoint fixtures, not only direct collector calls.

## Done when

- Text and JSON records contain the same required metadata fields without payloads, secrets, or raw headers.
- Every locally generated or forwarded response records its actual HTTP status when one was written.
- A canceled active request is included in the summary before `WriteSummary` returns.
- Additional usage fields prevent a complete-looking cost estimate.
- Mixed success, failure, cancellation, and model-list traffic still yields correct generation totals and parseable output.

## Depends on

- [08: Report request usage and a truthful session summary](08-request-accounting.md)
