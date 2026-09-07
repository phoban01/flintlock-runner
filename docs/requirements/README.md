# Writing requirements for flintlock-runner

This directory holds the normative specification for flintlock-runner. Every
requirement is written in EARS (Easy Approach to Requirements Syntax) and is
traced to code and tests with [duvet](https://github.com/awslabs/duvet).
Because duvet extracts requirements mechanically, the authoring rules below
are not stylistic preferences; breaking them silently loses requirements from
the coverage report.

## Documents

| File | Prefix | Scope |
|------|--------|-------|
| `00-glossary.md` | – | Defined terms used by every other document (non-normative) |
| `01-gitlab-protocol.md` | `GL` | How the Runner speaks to GitLab, built on the gitlab-runner Go packages |
| `02-executor.md` | `EX` | The flintlock executor and the Guest Transport that runs stages in a microVM |
| `03-scheduler.md` | `SC` | The scheduling component: capacity, profiles, allocation, placement, cleanup |
| `04-pool-manager.md` | `PL` | Integration with the flintlock warm pool manager (battery) |
| `05-orchestrator.md` | `BR` | Integration with flintlock hosts directly and through the orchestrator (brigade) |
| `06-fleet.md` | `FL` | Standing up and operating a fleet of EC2 bare-metal hosts |
| `07-configuration.md` | `CF` | The configuration surface |
| `08-observability.md` | `OB` | Logging, metrics, health, job-log annotations |
| `09-security.md` | `SE` | Isolation, secrets, transport security |

## EARS patterns

Use exactly one of these shapes per requirement. The subject is always one of
the defined system names from the glossary (the Runner, the Executor, the
Scheduler, the Guest Transport, the Fleet Controller).

| Pattern | Shape |
|---------|-------|
| Ubiquitous | The `<system>` SHALL `<response>`. |
| Event-driven | When `<trigger>`, the `<system>` SHALL `<response>`. |
| State-driven | While `<state>`, the `<system>` SHALL `<response>`. |
| Unwanted behaviour | If `<condition>`, then the `<system>` SHALL `<response>`. |
| Optional feature | Where `<feature is configured>`, the `<system>` SHALL `<response>`. |
| Complex | Any combination of the above clauses before a single SHALL. |

## Rules imposed by duvet

1. **Keywords are uppercase.** duvet only recognises `MUST`, `MUST NOT`,
   `SHALL`, `SHALL NOT`, `SHOULD`, `SHOULD NOT`, `MAY`, `REQUIRED`,
   `RECOMMENDED`, `OPTIONAL`. `SHALL` maps to level MUST. Lowercase `shall`
   is not extracted. Never use these words in uppercase in explanatory prose.
2. **One sentence is one requirement.** duvet cuts requirements at a full
   stop followed by whitespace. Write each requirement as a single sentence
   and never use `e.g. `, `i.e. ` or `etc. ` inside a requirement. Dots not
   followed by whitespace (`config.toml`, `v0.11.0`) are safe.
3. **One requirement per bullet.** Each requirement is a list item beginning
   with its bold identifier, for example `- **SC-004** When ...`. A bullet
   without a keyword is not a requirement, so a lead-in sentence followed by
   sub-bullets does not produce one requirement per sub-bullet; write the
   ordering or the alternatives inline instead.
4. **Sections have stable identifiers.** Every heading carries an explicit
   pandoc-style attribute such as `## Job acquisition {#job-acquisition}`.
   Annotations target `docs/requirements/<file>.md#<id>`, so renaming a
   heading without keeping its id breaks every citation under it.
5. **Identifiers are never reused.** When a requirement is retired, mark it
   `(withdrawn)` and keep the number; the next requirement takes the next
   free number in that prefix.
6. **Explanatory prose is separate.** Rationale, examples and background go
   in paragraphs outside the bullet lists and contain no uppercase keywords.

## Annotating code

Implementation:

```go
//= docs/requirements/03-scheduler.md#capacity
//# The Scheduler SHALL refuse a reservation when no capacity is available.
func (s *Scheduler) Reserve(ctx context.Context) (*Reservation, error) {
```

Test (always give `type=test` explicitly, because Go tests live next to the
code they test and are matched by the same source glob):

```go
//= docs/requirements/03-scheduler.md#capacity
//= type=test
//# The Scheduler SHALL refuse a reservation when no capacity is available.
func TestReserveRefusesWhenFull(t *testing.T) {
```

Other annotation types: `type=exception` with a `reason=` line for a
requirement deliberately not met, `type=implication` for a requirement that
is satisfied by construction, and `type=todo` with `tracking-issue=` for
planned work. The quoted text after `//#` has to be a contiguous substring of
the section; whitespace differences are tolerated.

## Running duvet

```sh
cargo install duvet --locked   # once
make duvet                     # writes .duvet/reports/report.html and refreshes .duvet/snapshot.txt
make duvet-ci                  # fails if the snapshot differs from the committed one
```

Extracted requirements land in `.duvet/requirements/` and are regenerated on
every run; they are not edited by hand. Renaming a section leaves a stale
file behind that makes `duvet report` fail with a missing-section error,
which is why the Makefile clears the directory before each run.
