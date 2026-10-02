---
name: pre-push
description: Run the repo's CI checks (gofmt, go vet, the pinned golangci-lint v1.59.1 under Go 1.22, unit tests) before every git push, commit that will be pushed, or PR create/update in the OpenRiak operator repo. Use ALWAYS before pushing; skipping it is why lint keeps failing in CI.
---

# Pre-push checks

CI lint has failed repeatedly on things `go vet` and `gofmt` do not catch (`lll`, `prealloc`,
`goconst`, `unparam`). The cause was never running the real linter locally: a globally installed
`golangci-lint` is a newer major version that cannot read `.golangci.yml`, and a newer local Go
cannot build or run the pinned v1.59.1. So: **never push without this.**

## Do this before every push / PR create / PR update

```bash
hack/pre-push.sh          # gofmt, go vet, golangci-lint v1.59.1 on Go 1.22, unit tests
FAST=1 hack/pre-push.sh   # same, skipping the slow envtest controller tests (still run them before the final push)
```

- It must exit 0. If it fails, fix the cause and rerun; do not push "to let CI tell me".
- The first run installs the pinned golangci-lint into `~/.cache/openriak-operator/bin` (about a minute
  and a half, needs network); later runs, including from other git worktrees, reuse it.
- Run it from the checkout or worktree you are pushing from.
- Do not "check" with `go vet` / `gofmt` alone, and do not use a globally installed `golangci-lint`.
- Never hide an exit status behind a pipe (`go test ... | tail`): a failing test then looks like success.
  Run the command bare, or use `set -o pipefail` / `if cmd; then ...`.

## What the linters in `.golangci.yml` bite on (fix, don't suppress)

| Linter | Typical trigger | Fix |
|---|---|---|
| `lll` | a line over 120 chars (applies to `cmd/`, `test/`; not `api/`, `internal/`) | wrap the string / call arguments |
| `prealloc` | `var xs []T` filled in a loop | `xs := make([]T, 0, n)` |
| `goconst` | the same string literal 3+ times | name a constant |
| `unparam` | a parameter that always receives the same value, or an unused result | drop the parameter |
| `gocyclo` | a function with too many branches (keep `Reconcile` small) | extract helpers |
| `revive` | comment spacing (`//foo`) | `// foo` |
| `gofmt`/`goimports` | unformatted file or import order | `gofmt -w`, fix imports |

Only suppress with `//nolint:<linter> // reason` when the finding is genuinely a false positive, and say why.

## If the tooling itself fails

- `go: downloading go1.22.12` on first use is expected (Go fetches the toolchain).
- `invalid array length` building golangci-lint means the pinned toolchain was not used: run through
  `GOTOOLCHAIN=go1.22.12 make lint` (the script does).
- `unsupported version of the configuration` means a global `golangci-lint` was invoked instead of the
  pinned one in `$LOCALBIN`.
