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

.PHONY: build test lint roundtrip generate demo dashboard-demo clean install

VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/polyql ./cmd/polyql/

test:
	go test ./... -race -count=1

lint:
	golangci-lint run

roundtrip:
	go test ./pkg/compiler/ -run TestRoundTrip -v -count=1

generate:
	go generate ./...

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

clean:
	rm -rf bin/ coverage.out

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/polyql/
