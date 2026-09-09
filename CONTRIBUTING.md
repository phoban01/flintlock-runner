# Contributing to flintlock-runner

This project is built from a normative specification: every requirement in
`docs/requirements/` is traced to code and tests with
[duvet](https://github.com/awslabs/duvet), and a pull request is done when
duvet can see that each requirement it owns is implemented and tested. This
page is the short version of the rules; `docs/requirements/README.md` is the
long one and `docs/PLAN.md` is the coordination contract.

## How work is split

- **The unit of assignment is the requirement ID** (`SC-020`, `TD-001`, ...).
  Each GitHub issue lists the IDs it owns; no two open issues claim the same
  ID. The issue assignee owns them until the PR merges.
- **Branches are `wp/<key>`**, one per work package from the table in
  `docs/PLAN.md` (`wp/fakes`, `wp/poolmgr`, `wp/hosts`, `wp/scheduler`,
  `wp/config`, `wp/executor`, `wp/fleet`). One branch, one worktree, one PR.
  Nobody pushes to `main`.
- **Interfaces change through one PR.** `internal/*/interfaces.go` and
  `internal/config/types.go` are owned by the lead. If you need a change,
  open a small PR against those files first; do not widen an interface
  inside a feature PR.
- **Package boundaries** are the ones in `README.md`. gitlab-runner is
  imported only by `internal/executor`, by `internal/config/runnercfg` (which
  holds the `RunnerConfig` translation so that `internal/config` itself stays
  free of it), by the GitLab test doubles under `internal/testing`, and by
  `cmd/flintlock-runner`; `internal/flintlock` and
  `internal/poolmgr` are the only packages that import the flintlock and
  battery generated code. Nothing on the Runner side holds a
  `flintlock.PoolHostClient` (HO-007).

## Definition of done for a PR

1. Every owned ID has an **implementation citation and a test citation** in
   duvet's JSON report. `hack/duvet-coverage.sh <IDs>` says so.
2. `go build ./...`, `go vet ./...`, `golangci-lint` and `go test ./...`
   pass (`make build vet lint test`).
3. The end-to-end scenarios the issue names pass (`make e2e`, once the
   harness exists).
4. No new `// TODO` without a `type=todo` duvet annotation and a tracking
   issue.
5. The PR body has a line `Owns: <ID> <ID> ...` listing the IDs it claims.
   CI runs the coverage gate over exactly that list. Ranges such as
   `TD-001..010` are accepted. A pull request that implements no requirement,
   such as a tooling or CI change, says `Owns: none` and the gate is skipped.

`.duvet/snapshot.txt` is regenerated and committed only at milestones, by
the lead. `make duvet` rewrites it locally; restore it with
`git checkout origin/main -- .duvet/snapshot.txt` before you commit, because
CI rejects a pull request that changes it. A milestone snapshot declares
itself with a line reading `Snapshot: milestone` in the pull request body.

## Citing requirements

Citations are comments. The reference names the document and the section id
(the `{#...}` attribute on the heading), and the quoted text has to be a
contiguous substring of that section; whitespace differences are tolerated.

Implementation:

```go
//= docs/requirements/03-scheduler.md#capacity
//# When a Reservation is requested and available capacity is zero, the
//# Scheduler SHALL refuse the Reservation.

// Reserve grants a Reservation when capacity allows (SC-003, SC-004).
func (s *impl) Reserve(ctx context.Context) (*Reservation, error) {
```

Test. Always give `type=test` explicitly, because tests live next to the
code and are matched by the same source glob:

```go
//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# When a Reservation is requested and available capacity is zero, the
//# Scheduler SHALL refuse the Reservation.

func TestReserveRefusesWhenFull(t *testing.T) {
```

Other types: `type=exception` with a `reason=` line for a requirement
deliberately not met, `type=implication` for one satisfied by construction
(for example by an imported gitlab-runner package), and `type=todo` with
`tracking-issue=` for planned work. A `todo` does not satisfy the gate.

**Keep annotation blocks out of doc comments.** `gofmt` rewrites doc
comments and turns `//=` into `// =`, which duvet no longer recognises. Put
the annotation block above the declaration's doc comment with a blank line
between them, as in the examples above, or inside the function body. The
skeleton packages under `internal/` show the layout.

Rules that keep requirements extractable, from `docs/requirements/README.md`:
keywords are uppercase and only appear in requirement bullets; one sentence
is one requirement; one requirement per bullet, starting with its bold
identifier; headings carry stable `{#id}` attributes; identifiers are never
reused once cited (retired ones are marked withdrawn).

## Running the gate locally

```sh
cargo install duvet --locked          # once; CI pins 0.4.3
make duvet                            # .duvet/reports/report.{html,json}
hack/duvet-coverage.sh SC-001 SC-002  # or: make coverage-gate IDS="SC-001 SC-002"
```

The script runs `duvet report`, reads `.duvet/reports/report.json` and
prints one line per ID: `ok` when both citations are present, `FAIL` with
what is missing otherwise. Set `SKIP_REPORT=1` to reuse an existing report
and `DUVET=/path/to/duvet` to point at a binary that is not on `PATH`.

## Toolchain

`go.mod` pins gitlab-runner to a commit and declares the Go version that
commit requires; with `GOTOOLCHAIN=auto` (the default) the `go` command
downloads it. `make lint` runs golangci-lint at the version pinned in the
Makefile through `go run`, so nothing else needs installing.

## Commit messages

Describe what changed and why in the imperative. Every commit made with an
AI assistant carries the assistant's `Co-Authored-By` trailer.
