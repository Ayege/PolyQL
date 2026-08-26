# PolyQL — observability query translator.

BINARY     := polyql
CMD        := ./cmd/polyql
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
           -X main.version=$(VERSION) \
           -X main.commit=$(COMMIT) \
           -X main.buildDate=$(BUILD_DATE)

.PHONY: build test lint roundtrip generate demo dashboard-demo playground playground-serve clean install

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/polyql ./cmd/polyql/
	go build -ldflags "$(LDFLAGS)" -o bin/polyql-proxy ./cmd/polyql-proxy/

test:
	go test ./... -race -count=1

lint:
	golangci-lint run --skip-config-verify

roundtrip:
	go test ./pkg/compiler/ -run TestRoundTrip -v -count=1

generate:
	go generate ./...
	go run ./internal/playground/gen web/examples.js

demo: build
	@echo "Generating live terminal demo GIF using VHS..."
	vhs assets/demo.tape

dashboard-demo:
	@echo "Translating sample dashboard PromQL → LogQL..."
	go run ./cmd/polyql/ dashboard translate \
		--from promql --to logql \
		--input testdata/dashboards/sample_promql.json \
		--report-format text
	@echo "Done."

# playground builds the browser translator: the compiler as WebAssembly, plus the
# wasm_exec.js shim from the toolchain that built it. The shim is version-matched
# to the compiler, so it is copied rather than committed — a stale copy fails at
# runtime, in the browser, where it is hardest to diagnose.
playground:
	go run ./internal/playground/gen web/examples.js
	GOOS=js GOARCH=wasm go build -ldflags "$(LDFLAGS)" -o web/polyql.wasm ./cmd/polyql-wasm/
	install -m 0644 "$(shell go env GOROOT)/lib/wasm/wasm_exec.js" web/wasm_exec.js
	@echo "built web/ — serve it with 'make playground-serve'"

playground-serve: playground
	@echo "http://localhost:8080 — ctrl-c to stop"
	@cd web && python3 -m http.server 8080

clean:
	rm -rf bin/ coverage.out web/polyql.wasm web/wasm_exec.js

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/polyql/
	go install -ldflags "$(LDFLAGS)" ./cmd/polyql-proxy/
