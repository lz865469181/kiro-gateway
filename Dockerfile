# Kiro Gateway - cross-platform Go image
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/kiro-gateway ./cmd/kiro-gateway
RUN mkdir -p /out/data/debug_logs && chown -R 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/kiro-gateway /app/kiro-gateway
COPY --from=build --chown=65532:65532 /out/data /data
ENV ACCOUNTS_CONFIG_FILE=/data/credentials.json \
    ACCOUNTS_STATE_FILE=/data/state.json \
    DEBUG_DIR=/data/debug_logs
VOLUME ["/data"]
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 CMD ["/app/kiro-gateway", "healthcheck"]
ENTRYPOINT ["/app/kiro-gateway"]
