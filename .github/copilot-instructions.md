# Copilot Instructions

## Go Version & Style

- Go 1.26+.
- Follow standard `gofmt` / `goimports` formatting.
- Ensure `golangci-lint` wsl_v5 and noinlineerr compliant.

## Implementation rules

- Read the package you are editing and follow its existing patterns for naming, options, error handling, comments, and tests.
- Keep public APIs backward compatible unless the task explicitly requires a breaking change.
- Prefer idiomatic Go and the repository's established patterns over inventing new abstractions.
- Prefer minimal, surgical changes. Avoid requesting speculative flexibility, broad refactors, or unrelated cleanup when the diff can stay focused on the goal.
- Write comments only for intent, invariants, and non-obvious constraints. Do not add comments that restate the code.
- Preserve existing code comments where it makes sense and still valid reducing the diff or changeset so it is easier to review.
- Do not use excessive defensive over-engineering to minimize and reduce the diff or changeset so it is easier to review.
- Do not add or remove new lines to existing code that is not changing to reduce the diff or changeset so it is easier to review.
- Aim for zero-allocation (or single allocation) for memory allocation and memory usage optimizations.
- Prevent time-of-check to time-of-use (TOCTOU) races.
- Prefer json.Unmarshal() over json.NewDecoder(strings.NewReader()) patterns.

