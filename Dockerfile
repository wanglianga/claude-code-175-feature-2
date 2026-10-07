# ---- build ----
FROM golang:1.19-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# migrations/ 与 web/(openapi.yaml + swagger-ui) 通过 go:embed 打进二进制
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/deposit-server ./cmd/server

# ---- runtime ----
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl postgresql-client \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -r -u 10001 appuser
WORKDIR /app
COPY --from=build /out/deposit-server /app/deposit-server
USER appuser
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=10 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/deposit-server"]
