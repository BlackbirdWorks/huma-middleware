# AGENTS.md - the rules of the huma-middleware repository

The single place to read before changing this repository (contributors and coding agents alike). Tasks are run with
[Task](https://taskfile.dev): `task` lists them.

## What this is

Reusable, router-agnostic middleware for [huma v2](https://github.com/danielgtaylor/huma): `middleware/cleanpath` (a
`net/http` middleware that cleans the path before routing), `middleware/tracing` (OpenTelemetry through `otelhttp`),
`middleware/logging` (`log/slog` request logger), `middleware/accesslog` (the allocation-free access log), `middleware/recovery` (panics) and
`middleware/compression` (zstd, brotli, gzip). One Go module, MIT licensed.

## Design

- Every middleware is a generic huma middleware, `func(ctx huma.Context, next func(huma.Context))`, written against the
  `huma.Context` interface only. No router adapter (`humago`, `humachi`, `humagin`, ...), no `Unwrap`, no `humatest` in
  non-test code (`depguard` enforces it). A middleware must behave the same under every adapter.
- Plain `http.Handler` variants (`tracing.Handler`, `logging.Handler`) are allowed only as thin layers over the same core
  and import `net/http` only.
- Dependency isolation: `logging` and `recovery` use the OpenTelemetry API only (no SDK, no contrib); `accesslog` has
  no OpenTelemetry import at all (it gets trace IDs through `internal/reqlog`, the one thing it shares with `logging`);
  `tracing` has no compression library; `compression` and `cleanpath` have no OpenTelemetry. `depguard` enforces it.
  Ask before adding a dependency.
- Functional options, zero-config defaults. No `init()`, no package-level mutable state, no singletons
  (`gochecknoglobals`, `gochecknoinits`, `reassign`). The one permitted global is `slog.Default()`, resolved only where
  no logger was given: in `logging` and `accesslog` on every request, in `tracing` once at construction for its one "tracing disabled"
  line, and in `recovery` through `logging.FromContext` when it has no logger of its own. A nil logger always means
  `slog.Default()`; nothing else reads a global logger.
- Log with `log/slog` and typed attributes (`LogAttrs`, `slog.String`, ...); never key/value pairs (`sloglint`).
  No `zap`, no `echo`, no stdlib `log`.
- Hot path: no `fmt`, no maps, no allocation that is not needed; benchmark with `task bench` before and after, compare
  with `benchstat` on pinned cores (build the test binaries of the base and of the branch once and alternate them) and
  keep an optimisation only when it measures better than the noise. Pooled objects (response wrappers, views, guards)
  are reset when taken, never given back after a panic or while a stream callback may still use them, and have a
  parallel `-race` test. What each middleware allocates on top of the handler is pinned by `AllocsPerRun` tests (they
  skip under the race detector, `task allocs` runs them).
- Return errors, do not panic. Public API gets doc comments and a runnable `Example` with `// Output:`.

## Tests

The rules below are checked by `internal/testrules` (`TestCheck`, run by `task lint`, by `task test` and so by CI).

- Every `Test*` function is a table test: `tests := []struct{ name string; ... }`, a `for _, tt := range tests` loop that
  calls `t.Run(tt.name, ...)`, and `t.Parallel()` at the top of the test and of every subtest. Each case builds its own
  API, recorder and exporter. A new case is a new row, never a new test function.
- One table test per exported symbol, named after it: `TestNew`, `TestHandler`, `TestWithContentType`,
  `TestFromContext`; `TestType_Method` for a method (`TestPanicError_Error`). All the cases of the symbol are its rows:
  happy paths, edge cases, errors, every adapter, and the regression of every review finding. A test file follows the
  source file it tests (`options.go` is tested in `options_test.go`); there are no catch-all files (`fixes_test.go`,
  `behaviour_test.go`). A helper is tested through the table of the exported symbol that uses it; only a helper that no
  exported symbol can reach gets a table of its own, in an internal test file (`*_internal_test.go`, same package,
  table-driven, named after the helper: `TestEscapedPath`) with the reason in its comment. Never `export_test.go`.
- Row names are short and lowercase ("multipart form given back", "no stack"), not sentences. Test function names are
  never descriptions of behaviour.
- Tests of more than one package (`TestStack`, `TestCombined`, `TestReadme`) live in `middleware/*_test.go` and are the
  only names that are not an exported symbol; they, and the `TestAllocs` of each middleware (all its allocation pins in
  one serial table), are listed with a reason in `repoRules` of `internal/testrules/testrules_test.go`.
- The checker fails when: a `Test*` has no `range` loop whose body calls `t.Run`; it does not call `t.Parallel()` and is
  not in the `Serial` list; its name (without `Test`) is not an exported symbol of the package under test, or
  `Type_Method`, and is not in the `Names` list; an exported function or method has no `Test` function of that name and
  is not in the `Untested` list; an entry of a list matches nothing (the lists cannot rot). Each entry of a list carries
  its reason. Fuzz targets and benchmarks are data-driven and exempt from the table form.
- The only non-parallel tests change process-wide state (`slog.SetDefault`, the global OpenTelemetry propagator) or
  assert allocations with `testing.AllocsPerRun` (it panics in parallel tests and would count other goroutines). They are
  still table tests, say why in a comment, carry a reasoned `//nolint:paralleltest` and are in the `Serial` list. The
  global is restored with `t.Cleanup`. Allocation pins skip under the race detector (`internal/racedetect`) and run in
  `task allocs`. Reuse of pooled objects is not tested through a `sync.Pool` (it may drop items): inject the store.
- `testify` only: `require` stops the test, `assert` continues; `t.Fatal*`/`t.Error*` are forbidden (lint).
- Black-box tests in `package x_test`; white-box tests live in `*_internal_test.go` and only when unavoidable.
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
