FROM --platform=$BUILDPLATFORM node:24.17.0-bookworm-slim AS web
WORKDIR /src/apps/web
COPY apps/web/package*.json ./
RUN npm ci --no-audit --no-fund
COPY apps/web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS go
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=1.0.0-dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/msboost-server ./cmd/server && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/msboost-agent ./cmd/agent && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/msboost-restore ./cmd/restore

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source="https://github.com/mozziexwz/node"
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates curl tzdata && rm -rf /var/lib/apt/lists/* && useradd --system --uid 10001 --create-home msboost && mkdir -p /app/data && chown 10001:10001 /app/data
WORKDIR /app
COPY --from=go /out/ /usr/local/bin/
COPY --from=web /src/apps/web/dist/ ./apps/web/dist/
COPY installers/ ./installers/
# Release archives, private source staging and Git checkouts may carry
# different file modes. The unprivileged server only reads these public,
# checksum-pinned scripts; make that permission deterministic in the image.
RUN chmod -R a+rX /app/installers
USER 10001:10001
ENV LISTEN_ADDR=0.0.0.0:8080 DATA_DIR=/app/data WEB_DIR=/app/apps/web/dist
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=6s --start-period=20s --retries=12 CMD curl --fail --silent --max-time 5 http://127.0.0.1:8080/api/health || exit 1
ENTRYPOINT ["msboost-server"]
