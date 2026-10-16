# Copilot Instructions

## Environment & Formatting

- Target: Go 1.26+.
- Use hard tabs for indentation and adhere strictly to standard `gofmt` / `goimports`.
- Enforce `golangci-lint` compliance:
  - `noinlineerr`: Never inline error assignment with checking (do not use `if err := ...; err != nil`). Always assign `err` on its own line first.
  - `wsl_v5`: Maintain proper statement separation, particularly whitespace preceding variable assignments and blocks.

## Diff Hygiene & Scope

- Keep changes minimal, surgical, and scoped strictly to the task.
- Do not add, remove, or modify blank lines or whitespace in untouched lines.
- Preserve existing comments unless they are invalidated by the change.
- Never introduce speculative flexibility, broad refactoring, or unrelated cleanups.
- Avoid defensive over-engineering (e.g., unnecessary nil-guards, redundant abstractions, or deep interface wrapping) where standard repository flow suffices.
- Keep public APIs backward compatible unless breaking changes are explicitly requested.

## Idioms & Performance

- Zero-allocation mindset: Aim for zero (or single) allocations in hot paths and slice/buffer handling.
- Concurrency safety: Prevent time-of-check to time-of-use (TOCTOU) race conditions on shared state and filesystem operations.
- Serialization: Prefer `json.Unmarshal([]byte)` over `json.NewDecoder(strings.NewReader())`.
- Comments: Document only intent, invariants, and non-obvious constraints. Never re-state what the code obviously does.
- Consistency: Match the existing package's conventions for naming, options, error handling, and signatures.
