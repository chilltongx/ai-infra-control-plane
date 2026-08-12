# Agent Instructions

## Commands

| Task | Command |
| --- | --- |
| Unit tests | `go test ./...` |
| Concurrency tests | `go test -race ./...` |
| Static analysis | `go vet ./... && staticcheck ./...` |
| Full local check | `make check` |
| Build binaries | `make build` |

## External References

| Need | File |
| --- | --- |
| Setup and status | `README.md` |
| Architecture and invariants | `docs/design.md` |
| HTTP contract | `docs/api.md` |
| Component diagram | `docs/control-plane.architecture.json` |
| Run lifecycle | `docs/run-lifecycle.lifecycle.json` |

## Key Conventions

- The control plane schedules; only workers execute recipes.
- Keep public recipes typed. Never add a general shell-command adapter.
- Every attempt mutation must validate attempt ID, lease token, and fence.
- Preserve durable completion receipts and their replay/conflict semantics.
- Never persist or log plaintext lease tokens.
- Retries create new attempts; do not revive terminal attempts.
- JSONStore is single-process development storage; do not imply HA semantics.
- Preserve the strict API JSON decoder and bounded request bodies.
- Keep built-in executors dependency-free and test their argv construction.
- Regenerate Archify HTML from its JSON source; do not edit generated diagrams.
