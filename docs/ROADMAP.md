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

**Four of the five are now closed.** Only the vendor DSLs remain, and those are a
community extension point rather than central work. The diagrams were updated
alongside each closure — `docs/diagrams_test.go` enforces it, and caught every
one of them.

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
| ~~1~~ | ~~Federation proxy~~ | L1, L2 | **DONE** — `polyql-proxy`, fail-closed, responses passed through | ~~L~~ |
| ~~2~~ | ~~OTel exporter~~ | L2, L3c | **DONE** — one span per translation, no-op without a collector | ~~M~~ |
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

## 1. Translating proxy — DONE

`cmd/polyql-proxy` in front of `pkg/proxy`. The backends' own endpoints, so a
client needs its address changed and nothing else:

```sh
polyql-proxy --source-dsl promql --to-dsl logql --upstream http://loki:3100
```

The three decisions this was blocked on, and what was chosen:

- **Lossy queries fail closed.** A query the target cannot fully express returns
  400 with the fidelity report as the body, and never reaches the backend.
  `--allow-partial` opts out per deployment. The gate is *completeness*, not
  losslessness: an approximation was written and explained, so it still asks the
  same question and is forwarded either way. Only an unsupported construct —
  something that was not written at all — is refused.
- **Responses pass through untouched.** The body is the upstream's own, in its
  own shape. Translating results is a second compiler roughly the size of the
  first; reshaping a few fields to look like one would be worse than not having
  it. The README says so plainly rather than leaving it to be discovered.
- **Configuration is flags.** One route per process. A config file naming several
  upstreams is a larger surface and nobody has asked for it.

Two things fell out of building it that are worth recording:

- **The policy lives in `Translator`, with no transport.** That is what makes the
  fail-closed decision testable without a server, and what stops it being
  re-derived slightly differently in each handler.
- **Grafana sends long queries in a form body, not the URL.** Reading only the
  URL would have passed exactly the queries most likely to be lossy straight
  through untranslated. Both are read, and a re-encoded form gets a corrected
  `Content-Length` — a stale one truncates it.

Still open: gRPC (the diagrams once said "gRPC + HTTP"; only HTTP is built, and
L2 now says so), several upstreams in one process, and response translation.

---

## 2. OTel exporter — DONE

One span per translation, named `compiler.Translate`, carrying the source and
target languages, IR node count, fidelity score, the per-verdict counts and
whether the signal class mismatched.

```sh
polyql --otlp-endpoint http://collector:4318 translate --from promql --to logql --query up
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318   # the standard variable also works
```

The dependency question resolved differently from how the first draft of this
document framed it. **The stated design property is "single static binary, no
external files at runtime" — that is about runtime, not dependency count**, and
adding the SDK does not touch it. So the SDK went in directly.

What did matter was *where*:

- **The library imports the OTel API, never the SDK.** Without a provider
  installed, `otel.Tracer` returns a no-op, so an embedder importing
  `pkg/compiler` pays nothing and configures nothing. `pkg/telemetry` is the only
  package that imports the SDK, and only the commands import it.
- **A lossy translation is a successful span.** Marking it an error would make
  every honest fidelity report look like a broken request. Only a translation
  that could not run at all sets an error status.
- **The proxy continues an incoming trace** rather than starting its own, so a
  translation is one hop in the caller's trace.
- **The SDK's own failures are routed**, not left on the standard logger — this
  command keeps stderr readable next to a piped stdout.

An unreachable collector never interrupts a translation: the failure is reported
and carried past.

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

## Where things stand

1. ~~**stdin** (XS)~~ — **done**.
2. ~~**Grafana API input** (S)~~ — **done**.
3. ~~**OTel exporter** (M)~~ — **done**.
4. ~~**Proxy** (L)~~ — **done**, HTTP only.
5. **Vendor DSLs** — open, and deliberately a community extension point.

### What building these changed elsewhere

Two structural changes came out of the work rather than being planned:

- **`pkg/compiler.Translate` now exists.** Nothing assembled the pipeline in one
  place; the CLI, the dashboard translator and the corpus test each rebuilt
  parse → resolve → validate → emit. The proxy needed a transport-free
  translation and the OTel work needed a single instrumentation point, and both
  wanted the same function. Every caller now drives it, including the corpus —
  a suite that exercised its own private copy would pass while the shipped path
  broke.
- **`net/http` moved the vulnerability floor twice.** Grafana input took
  `govulncheck` from 0 to 12 findings; the toolchain went 1.25.8 → 1.25.13 to
  clear them. Worth expecting on any future work that opens a socket.

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
