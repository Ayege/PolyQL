# Contributing to PolyQL

Thanks for looking. The most valuable contribution you can make is **a new query
language** — the compiler was built to be extended that way, and every seam it
needs is documented below.

## Getting set up

```sh
git clone https://github.com/polyql/polyql
cd polyql
make test        # go test ./... -race
make build       # ./bin/polyql
```

Go 1.25 or later. No other tooling is needed to build or test;
[golangci-lint](https://golangci-lint.run) is needed for `make lint`.

## How to contribute

Fork, branch, open a pull request. Small pull requests get reviewed faster than
large ones, and a pull request that changes behaviour and formatting at once is
hard to review at all — please split them.

If you are planning something substantial, open an issue first. That is not
bureaucracy: a change to the IR ripples through the resolver, the validator and
both emitters, and it is much easier to agree on the shape before the code
exists.

## Adding a new DSL

This is the main extension path, and it needs no changes to the compiler core.

### 1. Write the language definition

Create `registry/{dsl}.yaml`. It tells the resolver what each of the language's
functions and operators means in IR terms, and tells the validator what the
language can and cannot express.

```yaml
dsl: traceql
signal_types: [span]

capabilities:
  joins: true
  join_types: [INNER]
  subqueries: false
  pipeline_ordered: false
  window_alignments: [UTC_NORMALIZED]
  bool_modifier: false

normalizations:
  aggregation_clause_position: before_operand
  duration_format: largest_unit
  string_quoting: double

type_coercion:
  spanset: STRING
  scalar: DOUBLE

operators:
  "=":  { ir_op: EQ,  context: selector }
  ">":  { ir_op: GT,  context: comparison }

functions:
  rate:
    ir_kind: RATE          # omit for a function with no IR aggregation operator
    agg_scope: TEMPORAL    # TEMPORAL collapses over time, GROUP across series
    arity: 1
    arg_types: [spanset]
    return_type: DOUBLE
```

Two rules are worth stating outright, because getting them wrong produces a
translator that lies:

- **Omit `ir_kind` when there is no honest equivalent.** LogQL's `bytes_rate`
  measures payload size, not entry count. Mapping it to IR `RATE` would make a
  translation look exact when it is not. Without `ir_kind` it becomes a function
  stage, and the validator reports it per target.
- **`agg_scope` is not optional decoration.** It is the only thing separating
  PromQL's `sum` from its `sum_over_time`; both are IR `SUM`.

Loading is strict — an unknown key or an unrecognised IR symbol fails with the
file and field named. Check your file before writing any Go:

```sh
polyql registry validate --dir ./registry
polyql registry diff --dir ./registry   # what it changes versus the built-in set
```

### 2. Write the parser

`pkg/compiler/parser/{dsl}/`, implementing `parser.Parser`:

```go
func (Parser) Parse(input string) (ast.Node, error)
func (Parser) DSL() string
func init() { parser.Register(Parser{}) }
```

The AST is **yours**, not a shared one. PromQL's tree and LogQL's tree look
nothing alike, and flattening them into a common grammar would lose exactly the
structure the resolver needs. What is shared is `ast.Node`: `String()` and
`DSL()`.

`String()` must render back to valid text in your language. `parser_test.go`
needs at least ten cases proving it, each parsing, rendering and re-parsing to
the same tree. That property is what makes the round-trip corpus possible.

**Arity and argument types belong here**, not in the validator. The parser has
the source text and can say `1:6: parse error: argument 1 of "rate" expects type
range vector`; the validator sees only IR nodes and could not point at anything.

### 3. Write the resolver mapping

`pkg/compiler/resolver/resolve_{dsl}.go` walks your AST and builds IR, reading
your registry definition for names and operators.

The IR is a **flat pipeline** where most languages nest. `sum by (job) (rate(x[5m]))`
becomes `[RATE/TEMPORAL, SUM/GROUP]`, innermost first. Fold each enclosing node
into the query its operand produced rather than nesting queries.

The resolver never sets a translatability flag. Every node it produces is `FULL`;
a construct it cannot represent is an error, not a quiet degradation.

### 4. Write the emitter

`pkg/compiler/emitter/{dsl}/`, implementing `emitter.Emitter`:

```go
func (Emitter) Emit(query *ir.Query, reg *registry.Registry) (string, error)
func (Emitter) DSL() string
func init() { emitter.Register(Emitter{}) }
```

The emitter reverses the flattening, and reads the flags the validator left
rather than judging anything itself. When a node is `UNSUPPORTED`, render what
you can and record a note — `emitter.Notes` writes them as comment lines above
the query, so the output still parses in your language.

### 5. Add round-trip cases

`testdata/{dsl}_to_{other}/` and `testdata/{other}_to_{dsl}/`, one YAML file per
case:

```yaml
name: 'rate over a range'
description: 'The canonical temporal aggregation.'
source_dsl: traceql
target_dsl: promql
input: 'rate({name="GET /api"}[5m])'
expected_output: 'rate(...)'
expected_fidelity_score: 0.8000
expected_flags:
  - path: 'Query.Pipeline[0].AggregationStage'
    flag: FULL
notes: 'Why this case is here and what it proves.'
```

The runner drives each case through the whole pipeline and **re-parses the result
with the target's own parser**. A translation that does not parse is not a
translation.

Do not hand-write the expected values and hope. Run the pipeline, read what it
produced, satisfy yourself it is *correct* — then record it.

### 6. Finish up

```sh
go generate ./...              # sync registry/ into pkg/registry/data/
make test                      # go test ./... -race
make lint
```

Then update the supported-DSL table in [README.md](README.md).

## Code style

- `gofmt` is the whole style guide. `make lint` runs the rest.
- Comments explain *why*, not *what*. The code already says what it does.
- No `TODO` without an issue link. An unlinked `TODO` is a wish, not a plan.
- Errors say what could not be done and what to do about it. `unknown source
  language "foo" (available: logql, promql)` beats `invalid DSL`.

## Test requirements

Every pull request needs tests. Beyond that:

| Change | Additional tests required |
|--------|---------------------------|
| Parser or emitter | Round-trip cases: parse → render → re-parse to the same tree |
| IR node type | `ir.Walk` **and** `ir.InspectPath` must handle it; `TestInspectPathMatchesWalk` pins them together |
| Resolver | A case in `testdata/`, with the expected values verified rather than guessed |
| Validator | A fidelity report test showing the flag *and* the reason |
| Registry schema | A rejection test — the loader is strict on purpose |

The suite treats a silent loss as a bug. If a translation drops something, some
test must fail.

## Commit conventions

```
type(scope): description
```

**Types:** `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`

**Scopes:** `parser`, `emitter`, `ir`, `resolver`, `validator`, `fidelity`,
`cli`, `dashboard`, `registry`

```
feat(parser): add TraceQL structural operators
fix(emitter): keep group_left label list in PromQL output
test(fidelity): cover a node drawing two verdicts
```

## Issue labels

| Label | Meaning |
|-------|---------|
| `good-first-issue` | Self-contained, with the approach already sketched |
| `new-dsl` | Adding or extending a query language |
| `parser` / `emitter` | Front and back ends |
| `ir` | The shared model — expect a longer review |
| `validator` / `fidelity` | Translation honesty; reviewed carefully |
| `cli` / `dashboard` | User-facing surfaces |
| `docs` | Documentation |

## Release process

Releases are tag-driven. A maintainer tags `vX.Y.Z` on `main`; the release
workflow builds every platform through GoReleaser, generates checksums and
publishes a GitHub release with the changelog.

Before tagging: `make check-generate`, `make test`, `make lint`, and a smoke run
of the built binary.

## Code of conduct

This project follows the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).
