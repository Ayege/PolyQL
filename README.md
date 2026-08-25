# PolyQL

Translate observability queries across PromQL, LogQL, and vendor DSLs without losing the truth about what changed.

[![Go](https://img.shields.io/github/go-mod/go-version/polyql/polyql)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go Report Card](https://goreportcard.com/badge/github.com/polyql/polyql)](https://goreportcard.com/report/github.com/polyql/polyql)
[![Codecov](https://codecov.io/gh/polyql/polyql/branch/main/graph/badge.svg)](https://codecov.io/gh/polyql/polyql)

<img src="assets/demo.gif" alt="PolyQL live translation demo" width="100%" />

## What PolyQL does

PolyQL translates observability queries between PromQL, LogQL, TraceQL, and extensible vendor DSLs through a shared telemetry IR aligned to the CNCF QLS semantic specification. Every translation includes an honest fidelity report showing exactly what translated fully, what is lossy, and what is unsupported. It is built to answer a simple operational question: “Can I trust this translated query, and if not, why?”

## Why it exists

OpenTelemetry standardized how telemetry is ingested; query languages remain fragmented across metrics, logs, traces, and vendor-specific pipelines. The CNCF Query Language Standardization working group explicitly called out this gap in [cncf/toc#1770](https://github.com/cncf/toc/issues/1770): a common, reusable translation layer is future work that is not solved by the QLS effort itself. PolyQL exists to fill that gap without forcing teams to rewrite every query by hand when they move between backends.

Existing translators are usually single-pair and one-directional: a PromQL-to-LogQL tool, a TraceQL-only emitter, or a dashboard converter living inside a vendor product. PolyQL is different. It is general-purpose, IR-based, and bidirectional. Query languages are described by data rather than by hard-coded compiler branches, so adding a new DSL means a YAML registry plus parser and emitter work instead of editing the core pipeline.

## Quick start

```bash
# Install
go install github.com/polyql/polyql/cmd/polyql@latest

# Translate a single query live
polyql translate --from promql --to logql \
  --query 'rate(http_requests_total{status="500"}[5m])'

# Translate with full fidelity report
polyql translate --from promql --to logql \
  --query 'sum by (job) (rate(http_requests_total[5m]))' \
  --format json

# Translate a Grafana dashboard
polyql dashboard translate \
  --from promql --to logql \
  --input dashboard.json \
  --output translated.json \
  --report report.md --report-format markdown

# Use in CI — fail if any construct is unsupported
polyql translate --from promql --to logql --file queries.txt || exit 1
```

## Supported DSLs

| DSL     | Parse | Emit | Status            |
| ------- | ----- | ---- | ----------------- |
| PromQL  | ✅    | ✅   | Stable            |
| LogQL   | ✅    | ✅   | Stable            |
| TraceQL | 🚧    | 🚧   | Planned           |
| NRQL    | —    | —   | Community welcome |
| DQL     | —    | —   | Community welcome |

## Architecture

PolyQL follows a six-stage compiler pipeline: Parse → AST → Resolve → IR → Validate → Emit. The full C4 diagrams live in [docs/](docs/) and the IR model is documented in [docs/c4-level3b-ir-datamodel.mmd](docs/c4-level3b-ir-datamodel.mmd). The data-driven registry is the main extension point: adding a DSL means creating a YAML catalog and matching parser/emitter pair, without changing the compiler core.

## Fidelity reporting

Every translation prints a fidelity report with the raw node-by-node verdicts. The model is intentionally honest:

```text
$ polyql translate --from promql --to logql \
    --query 'sum by (job) (rate(http_requests_total{status=~"5.."}[5m]))'

sum by (job) (rate({__name__="http_requests_total", status=~"5.."}[5m]))

PolyQL fidelity report: promql → logql
────────────────────────────────────────
Total nodes:    8
Full:           6 (75.0%)
Partial:        1 (12.5%)
Unsupported:    1 (12.5%)
Fidelity score: 0.75

FLAGS
  FULL      exact semantic match
  PARTIAL   approximate but explained
  UNSUPPORTED no valid target equivalent
```

The three flags are `FULL`, `PARTIAL`, and `UNSUPPORTED`. A score measures structural fidelity only; it is a hint for comparison, not an automatic verdict. PolyQL tells you what it cannot do, not just what it can.

## CLI reference

PolyQL exposes a small command surface for interactive use and automation:

- `polyql translate` — translate a single query, file, or stdin stream
- `polyql dashboard translate` — translate a whole Grafana dashboard
- `polyql registry` — inspect or validate the bundled language registry

For the full flag set, run `polyql translate --help`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the contribution path, coding expectations, and release process.

## CNCF alignment

PolyQL is designed as a reusable tool rather than a reference architecture, aligned with the CNCF QLS semantic specification. Its IR data models follow the QLS sections covering data types, metrics, logs/events, spans, profiles, selection, time-based windowing, aggregation, and joins. The project targets CNCF Sandbox submission under TAG Operational Resilience, with the implementation shaped to be a general-purpose translation layer rather than a single-vendor product.

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE).
