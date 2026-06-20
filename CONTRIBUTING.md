# Contributing to processkit-go

Thanks for your interest in improving **processkit-go**.

## Prerequisites

- Go 1.25 or later (the minimum is pinned by the `go` directive in `go.mod`).

## Build and test

```sh
go build ./...
go test ./...
```

CI also runs the tests with the race detector (`go test -race ./...`) on Linux; it
needs cgo (a C compiler), so it is not required for the basic local loop.

Before opening a pull request, also run the formatter, vet, and the meta-linter
— CI gates on all three:

```sh
gofmt -l .         # must print nothing; run `gofmt -w .` to fix
go vet ./...
golangci-lint run  # config in .golangci.yml; install from https://golangci-lint.run
```

A [`Makefile`](Makefile) wraps the common loops — `make build`, `make test`,
`make race`, `make lint`, and `make ci` (fmt + vet + lint + test). Run `make help`
for the full list. The raw `go`/`gofmt` commands above work just as well.

## Conventions

- **Formatting** is owned by `gofmt` (tabs, canonical layout); `.editorconfig`
  mirrors it. Do not reformat code you are not changing.
- **Dependencies** are tracked in `go.mod` / `go.sum` — add them with `go get`,
  pin versions, and commit both files. `go mod tidy` keeps them honest. Prefer the
  standard library.
- **Errors** use `errors.Is` / `errors.As` (typed sentinels + structs); a non-zero
  exit is data (in `Result`), not an error, until a success-requiring verb asks.
- **Comments** explain *why*, not *what*. Exported identifiers carry doc comments
  beginning with their name; keep the public API surface minimal and intentional.

## Changelog

Every user-visible change ships its [`CHANGELOG.md`](CHANGELOG.md) entry in the
same change set, under `## [Unreleased]`. Write the bullet for a consumer of the
library, not the implementer. Pure internal refactors are exempt.

## Pull requests

- Keep changes focused; unrelated cleanups belong in their own PR.
- Ensure CI (build/test on Linux, Windows, macOS) passes.
- Fill in the pull-request checklist.
