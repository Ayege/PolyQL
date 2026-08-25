# PolyQL builds to a single static binary with the language definitions
# compiled in, so the runtime image needs nothing but the binary itself.

FROM golang:1.24-alpine AS builder

WORKDIR /src

# Dependencies are copied first so that a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /polyql ./cmd/polyql

FROM alpine:3.20

# Certificates are here for the federation proxy, which will need to reach
# backends over TLS. The translator itself makes no network calls.
RUN apk --no-cache add ca-certificates

# A translator has no reason to run as root.
RUN adduser -D -u 10001 polyql
USER 10001

COPY --from=builder /polyql /usr/local/bin/polyql

ENTRYPOINT ["polyql"]
CMD ["--help"]

LABEL org.opencontainers.image.title="polyql" \
      org.opencontainers.image.description="Observability query translator with fidelity reporting" \
      org.opencontainers.image.source="https://github.com/polyql/polyql" \
      org.opencontainers.image.licenses="Apache-2.0"
