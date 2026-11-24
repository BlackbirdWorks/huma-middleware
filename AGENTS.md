# AGENTS.md - the rules of the huma-middleware repository

The single place to read before changing this repository (contributors and coding agents alike). Tasks are run with
[Task](https://taskfile.dev): `task` lists them.

## What this is

Reusable, router-agnostic middleware for [huma v2](https://github.com/danielgtaylor/huma): `middleware/tracing`
(OpenTelemetry through `otelhttp`), `middleware/logging` (`log/slog` request logger and access log) and
`middleware/compression` (zstd, brotli, gzip). One Go module, MIT licensed.

## Design

- Every middleware is a generic huma middleware, `func(ctx huma.Context, next func(huma.Context))`, written against the
  `huma.Context` interface only. No router adapter (`humago`, `humachi`, `humagin`, ...), no `Unwrap`, no `humatest` in
  non-test code (`depguard` enforces it). A middleware must behave the same under every adapter.
- Plain `http.Handler` variants (`tracing.Handler`, `logging.Handler`) are allowed only as thin layers over the same core
  and import `net/http` only.
- Dependency isolation: `logging` uses the OpenTelemetry API only (no SDK, no contrib); `tracing` has no compression
  library; `compression` has no OpenTelemetry. `depguard` enforces it. Ask before adding a dependency.
- Functional options, zero-config defaults. No `init()`, no package-level mutable state, no singletons
  (`gochecknoglobals`, `gochecknoinits`, `reassign`). The one permitted use of a global is `slog.Default()` in
  `logging`, resolved per request when no logger was given.
- Log with `log/slog` and typed attributes (`LogAttrs`, `slog.String`, ...); never key/value pairs (`sloglint`).
  No `zap`, no `echo`, no stdlib `log`.
- Hot path: no `fmt`, no maps, no allocation that is not needed; benchmark with `task bench` before and after.
- Return errors, do not panic. Public API gets doc comments and a runnable `Example` with `// Output:`.

## Tests

- Table-driven and parallel: one `tests := []struct{ name string; ... }` per behaviour, `t.Parallel()` at the top of every
  test and every subtest, each case builds its own API, recorder and exporter. No copy-pasted near-identical tests.
- The only non-parallel tests are those that call `slog.SetDefault` (process-wide); they say so in a comment and
  restore the default with `t.Cleanup`.
- `testify` only: `require` stops the test, `assert` continues; `t.Fatal*`/`t.Error*` are forbidden (lint).
- Black-box tests in `package x_test`; white-box tests live in `*_internal_test.go` and only when unavoidable.
  Never add `export_test.go`.
- Every middleware is also tested under at least two real adapters (`humago` plus `humachi`) and `humatest`.
- No wall clock: time comes from an injected clock. No `time.Sleep`; wait on channels. No flaky tests: fix at the cause.
- Coverage is 100% of statements over all packages (`-coverpkg=./...`, `-race`), enforced by `task coverage` and CI.
  Reach it with real tests: injected failing writers and encoders, panic paths, pool paths. Never with coverage tricks.
  An unreachable statement is removed, not ignored.
- Fuzz seeds and `Example` functions count as tests.

## Lint

- `task lint` must report 0 issues: golangci-lint (version pinned in `Taskfile.yml`, config `.golangci.yml`, every linter
  of the sibling repositories at full strictness) plus `mulint`.
- No blanket or path-wide exclusions. A suppression is one line, `//nolint:<linter> // <specific reason>`; unused ones
  are removed (`nolintlint`). Never run `--fix` blindly; `golangci-lint fmt` for formatting only.
- `task tidy-check`, `task vulncheck` and `actionlint` / `zizmor` on the workflows must be clean.

## CI

Workflows pin every action by commit SHA, use least-privilege `permissions`, never use `pull_request_target`, and call
`task` targets. `ci-ok` is the single required check.

## Commits and pull requests

- Commits are signed; commit subjects and PR titles follow Conventional Commits.
- `main` is squash-merged. Never force-push, amend or rebase a shared branch.
- Verify every review finding, then fix it or answer it before merging.
- Never tag or release by hand.
