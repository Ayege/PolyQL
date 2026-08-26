# PolyQL roadmap — what the diagrams promise and the code does not yet do

The C4 diagrams in [docs/](.) describe the system PolyQL is aiming at, not only
the one that exists today. That is legitimate for an architecture diagram, but
it is only honest if the two are told apart — a diagram that quietly claims an
unbuilt component is the same class of mistake as a fidelity report that claims
an unbuilt translation.

Every element that does not exist in code is marked `[PLANNED]` and greyed in the
diagrams. This document is the other half: what each gap is, what it would take
to close it, and which decisions have to be made by a person before the work can
start.

**Two of the five are now closed** — stdin input and Grafana API input — and the
diagrams were updated the same day, which is the whole point. `docs/diagrams_test.go`
now enforces that: it fails when a component marked planned has since been built.

## How this audit was done

Each claim in each diagram was checked against the tree, not against memory:

| Check | Command |
| ----- | ------- |
| Is there any OpenTelemetry code or dependency? | `grep -rn "otel\|OTLP" --include="*.go"`, `go.mod` |
| Does anything speak HTTP? | `grep -rn "net/http" --include="*.go" pkg/ cmd/` |
| Does the proxy exist? | `git ls-tree -r HEAD --name-only \| grep proxy` |
| Which binaries ship? | `.goreleaser.yml`, `Dockerfile` |
| Do the IR types match the data model? | exported `type` decls vs. `class` decls in the diagram |
| Does the validator do what Level 3 says? | `grep -n "Arity\|ArgTypes" pkg/compiler/validator/*.go` |

## Summary

| # | Gap | Promised by | Exists today | Size |
| - | --- | ----------- | ------------ | ---- |
| 1 | Federation proxy | L1, L2 | `cmd/polyql-proxy/`, `pkg/proxy/` — both empty but for `.gitkeep` | **L** |
| 2 | OTel exporter | L2, L3c | nothing; the module has no OTel dependency | **M** |
| ~~3~~ | ~~Grafana HTTP API input~~ | L1, L2 | **DONE** — `--grafana-url` with `--dashboard-uid`, token from `GRAFANA_TOKEN` | ~~S~~ |
| 4 | Vendor DSLs (NRQL, DQL, InfluxQL) | L1 | none; the registry is the ready extension point | **M** each |
| ~~5~~ | ~~stdin input for `translate`~~ | README, CLI reference | **DONE** — a bare pipe, or `--file -` | ~~XS~~ |

Two further findings were **corrections, not gaps**, and are already fixed in
this pass:

- **Level 3 described a validator that does not exist.** It claimed the validator
  checks "function arity" and "type compatibility". It deliberately does not —
  `pkg/compiler/validator/validator.go` opens by explaining that arity and
  argument types were settled by the parser, which had the source text and could
  point at the offending token. Level 3c already said this correctly, so the two
  diagrams contradicted each other. Level 3 now matches the code.
- **Level 3b was missing a whole node family.** `FunctionStage.args` and
  `AggregationStage.parameter` are typed `IRExpr`, which the data model never
  declared, along with `LiteralExpr`, `RefExpr`, `QueryExpr`, `Timestamp`,
  `SortOrder` and `SignalMismatchInfo`. All are now present.

---

## 1. Federation proxy — size L

**Promised:** L1 shows a platform engineer federating queries over "Proxy
gRPC/HTTP", and PolyQL sending translated queries on to Prometheus, Loki and
Tempo. L2 gives it a container with a PromQL-compatible `/api/v1/query`.

**Exists:** nothing. `cmd/polyql-proxy/` and `pkg/proxy/` have contained only
`.gitkeep` since the initial commit. Nothing in the module imports `net/http`.

**Why it is the big one:** every other gap is additive. This one introduces a
network surface, a configuration model, an upstream failure mode and a
long-running process — none of which the codebase has today. The compiler
library is already shaped for it (`Emitter` and `Parser` both document that
implementations must be safe for concurrent use because "the federation proxy
shares one registered emitter across requests"), so the translation half is
ready. The serving half is not.

### Plan

**Phase 1 — `pkg/proxy`, transport-free.** A `Translator` that takes
`(sourceDSL, targetDSL, queryText)` and returns the translated text plus the
fidelity report, wrapping the existing pipeline. No HTTP. Fully unit-testable,
and it forces the lossy-translation decision below to be made in one place.

**Phase 2 — read-only HTTP, one backend pair.** `cmd/polyql-proxy` serving
Prometheus' `/api/v1/query` and `/api/v1/query_range`, translating the `query`
parameter and forwarding to a configured Loki upstream. Responses pass through
untouched (see the open decision). `httptest`-based tests; no live backend.

**Phase 3 — the other backends.** Loki's `/loki/api/v1/query_range` and Tempo's
`/api/search`. Tempo needs its own attention: TraceQL carries its time range in
request parameters rather than in the query text, so the proxy is the component
that has to move `start` and `end` across — which is exactly what the TraceQL
emitter's notes tell the caller to do today.

**Phase 4 — operability.** Health and readiness endpoints, a fidelity metric per
request, graceful shutdown, and the Dockerfile/goreleaser entries (both currently
build only `polyql`).

### Decisions needed first

- **What happens when a translation is lossy?** This is the product question, not
  an implementation detail. Fail closed with a 400, or forward the query and
  return the fidelity report in a response header? A tool whose stated purpose is
  refusing to hide loss probably fails closed by default with an explicit
  `--allow-partial`, but that is the maintainer's call.
- **Are responses translated too, or only queries?** Prometheus and Loki return
  different JSON shapes, so a genuinely transparent proxy would have to translate
  results as well — a second compiler, roughly the size of the first. Passing
  responses through is the honest v1, and the README should say so plainly.
- **How is a backend configured?** Flags for a single pair are enough for Phase 2;
  a config file naming several upstreams is a bigger surface and can wait.

---

## 2. OTel exporter — size M

**Promised:** L2 gives it a container emitting a span per translation with source
DSL, target DSL, IR node count, fidelity score and latency. L3c shows it as an
output of the validate stage.

**Exists:** nothing, and no dependency. Adding one matters more than usual here:
`go.mod` currently has exactly two direct dependencies, and the single static
binary with the registry compiled in is a stated design property. The OTel SDK is
a large tree.

### Plan

1. Define a tiny `Observer` interface in the compiler package — `TranslationDone(source, target string, nodes int, score float64, d time.Duration)` — with a no-op default. The compiler depends on the interface, never on OTel.
2. Put the OTel implementation in its own package, `pkg/telemetry/otel`, imported only by the commands.
3. Wire it behind a flag (`--otlp-endpoint`), off by default.

Keeping the dependency out of the library is the whole point: a project offering
itself as an embeddable translation layer should not force an OTel SDK on
everything that imports it.

---

## 3. Grafana HTTP API input — DONE

Shipped as `pkg/dashboard/grafana.go` plus two flags:

```sh
export GRAFANA_TOKEN=glsa_...
polyql dashboard translate --from promql --to logql \
  --grafana-url https://grafana.example.com --dashboard-uid abc123
```

Three decisions worth recording:

- **Read-only.** There is no push. Writing a translated dashboard back over the
  API would overwrite the panel expressions people are on call with, on the
  strength of a translation the fidelity report may well have flagged. A
  `POST /api/dashboards/db` belongs behind its own explicit command, if at all.
- **The token comes from the environment, not a flag.** An argument is visible in
  shell history and to anything that can list processes, and a dashboard-reading
  token is still a credential.
- **One parsing path.** `ReadDashboard` and the HTTP client both decode through
  `ParseDashboard`, so the file and API routes cannot drift.

One consequence worth recording, because it will recur for the proxy: **adding
`net/http` changed the vulnerability surface.** `govulncheck` went from 0 to 12
findings the moment the module started calling into `net/http`, `crypto/tls`,
`crypto/x509` and `net/url` — none of them reachable before. All were stdlib and
all cleared by moving the `go` directive from 1.25.8 to 1.25.13. Expect the same
when the proxy lands, and expect the toolchain floor to need moving with it.

Still open: fetching by *title* rather than UID, and listing a folder to
translate several dashboards at once. Neither is needed for the promise the
diagrams made.

---

## 4. Vendor DSLs — size M each

**Promised:** L1 lists NRQL, DQL and InfluxQL as vendor backends.

**Exists:** none. This is the least alarming gap, because the extension point is
real and documented: `CONTRIBUTING.md` explains the whole path, the registry
schema now carries the capability vocabulary needed to describe a language
honestly, and TraceQL is a worked example of adding one end to end.

**Plan:** none needed centrally. The README already lists these as "Community
welcome". The useful work is keeping the seam sharp — the parser/registry
consistency tests in `pkg/compiler/parser/registrytest/` are what stop a
definition drifting from the parser that feeds it.

---

## 5. stdin for `translate` — DONE

Both spellings now work — a bare pipe, and `--file -` to match the `-` that
`--output` already accepts for stdout:

```sh
cat queries.txt | polyql translate --from promql --to logql --format query-only
echo 'up'       | polyql translate --from promql --to logql --file -
```

Whether stdin is a pipe or a terminal is decided in `main`, not in the command:
a command that read an interactive terminal would hang waiting for input that is
never coming, and only the real process knows which it has.

Closing this exposed a latent bug worth noting. Three places used
`t.file == ""` to mean "the caller gave one inline query", which decided both the
JSON shape and the text layout. With stdin that condition is also true for a
pipe, so a piped stream would have rendered as a bare object rather than a list.
The meaning now lives in one predicate, `isSingleQuery`, keyed on `--query`
itself.

---

## Suggested order

1. ~~**stdin** (XS)~~ — **done**.
2. ~~**Grafana API input** (S)~~ — **done**.
3. **OTel exporter** (M) — blocked on a dependency decision, below.
4. **Proxy phases 1–2** (L) — blocked on the two decisions under G1.
5. **Proxy phases 3–4**, then vendor DSLs as they arrive.

### What the next two are waiting on

Neither remaining gap is blocked on effort. Both are blocked on a call that is
not the implementer's to make:

- **OTel:** adding the SDK takes `go.mod` from two direct dependencies to a large
  tree, against a stated design property of shipping one static binary with the
  registry compiled in. The `Observer` seam described above costs nothing and
  keeps the dependency out of the library — but building the seam with no
  exporter behind it is scaffolding, so it is worth doing *with* the decision
  rather than before it.
- **Proxy:** fail closed or forward-with-a-header on a lossy translation, and
  whether responses are translated at all. Both change what the component *is*,
  not how it is built.

## Keeping this honest — DONE

The guard proposed here now exists, in `docs/diagrams_test.go`:

- A component marked `[PLANNED]` must correspond to a package holding no Go
  source. This fails the day someone builds the proxy and forgets the diagram,
  which is the direction the drift actually runs.
- The OTel exporter may not be described as real while `go.mod` has no
  OpenTelemetry dependency — and, once it does, may not still be marked planned.
- `docs/` and `diagrams/` must stay byte-identical. Two copies of one file are a
  standing invitation to update one of them.

All three were verified to fail when the drift they describe is introduced, then
to pass once it is undone.
