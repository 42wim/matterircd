# Copilot Instructions — matterircd

## Environment & Formatting

- Target: Go 1.26+.
- Use hard tabs for indentation and adhere strictly to standard `gofmt` / `goimports`.
- Enforce `golangci-lint` compliance:
  - `noinlineerr`: Never inline error assignment with checking (do not use `if err := ...; err != nil`). Always assign `err` on its own line first.
  - `wsl_v5`: Maintain proper statement separation, particularly ensuring empty lines/whitespace precede variable assignments (avoid "invalid statement above assign") and control blocks.

## Diff Hygiene & Scope

- Keep changes minimal, surgical, and scoped strictly to the task.
- Do not add, remove, or modify blank lines or whitespace in untouched lines to keep diffs clean and easy to review.
- Preserve existing comments and structural flow unless invalidated by the change.
- Never introduce speculative flexibility, broad refactoring, or unrelated cleanups.
- Avoid excessive defensive over-engineering where standard repository flow suffices.
- Keep public APIs backward compatible unless breaking changes are explicitly requested.
- Do not write or suggest adding unit tests or test cases unless specifically requested.

## Idioms, Networking & Performance

- Zero-Allocation Mindset: Aim for zero (or single) allocations in hot networking paths, line parsers, slice slicing, and protocol message transformations.
- Concurrency & Deadlocks:
  - Check lock ordering and mutex scopes rigorously to prevent deadlocks.
  - Ensure goroutines terminate cleanly on connection drops or context cancellation; never leak goroutines or leave channels unbuffered if blocking is fatal.
  - Prevent time-of-check to time-of-use (TOCTOU) race conditions on shared state, session maps, and channel metadata.
  - Verify that changes do not introduce regressions into daemon connection loops.
- Serialization: Prefer `json.Unmarshal([]byte)` over `json.NewDecoder(strings.NewReader())`.
- Comments: Document only intent, invariants, protocol quirks, and non-obvious constraints. Do not restate obvious code.
- Consistency: Match the existing package conventions for naming, options, error handling, and method signatures.

## When Reviewing Pull Requests

- Lead with the most critical issue first. Do not bury blockers under minor nits or formatting notes.
- Distinguish blockers (must fix before merge) from suggestions (nice-to-have / non-blocking).
- Explain *why* a change is needed, not just *what* to change (e.g., protocol desync, race conditions, goroutine leaks, or memory growth).
- Reference project conventions from this document and the existing repository rather than invoking personal preferences.

## Review Tone & Feedback Style

- Direct and respectful. Avoid "you should" — prefer objective framing like "this function could", "consider", or "handling this here avoids a race condition".
- Provide concrete Go code examples in review comments whenever suggesting non-trivial logic, error-handling flows, or synchronization adjustments.

## What to Always Check

- Concurrency & Goroutine Lifecycles: Are mutexes unlocked in all exit paths? Could a channel send or receive block indefinitely on network teardown?
- Error Handling Completeness: Are network read/write errors, WebSocket disconnects, and API error payloads surfaced and handled without silently dropping state?
- Memory & Unbounded Growth: Do message caches, backlog queues, user maps, or channel buffers grow unbounded without eviction or size limits?
- Protocol Fidelity: Do changes adhere to IRC RFCs / Mattermost REST & WebSocket API expectations without breaking existing IRC client parsers?
- TOCTOU & State Consistency: Is shared state re-checked or held under lock across multi-step API and networking operations?
