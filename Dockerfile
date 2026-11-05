# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:24-alpine AS web-build
WORKDIR /src/web
COPY web/package*.json ./
RUN --mount=type=cache,id=l4d2panel-npm,target=/root/.npm,sharing=locked \
    npm ci --prefer-offline --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS go-build
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ARG ALPINE_MIRROR=https://dl-cdn.alpinelinux.org/alpine
RUN sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${ALPINE_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN --mount=type=cache,id=l4d2panel-go-mod,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=web-build /src/web/dist ./internal/control/static
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,id=l4d2panel-go-mod,target=/go/pkg/mod \
    --mount=type=cache,id=l4d2panel-go-build,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=${VERSION} -X github.com/l4d2-panel/l4d2panel/internal/version.Commit=${COMMIT} -X github.com/l4d2-panel/l4d2panel/internal/version.Date=${BUILD_DATE}" \
    -o /out/l4d2panel ./cmd/control

FROM alpine:3.22
ARG ALPINE_MIRROR=https://dl-cdn.alpinelinux.org/alpine
RUN sed -i "s|https://dl-cdn.alpinelinux.org/alpine|${ALPINE_MIRROR}|g" /etc/apk/repositories \
    && apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S panel \
    && adduser -S -D -H -G panel panel
WORKDIR /app
COPY migrations/sqlite ./migrations/sqlite
COPY docs ./docs
COPY --from=go-build /out/l4d2panel /usr/local/bin/l4d2panel
RUN mkdir -p /app/data /app/spool /app/updates && chown -R panel:panel /app
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="L4D2 Panel Control" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"
USER panel:panel
EXPOSE 19090
ENV LISTEN_ADDR=:19090 DATABASE_PATH=/app/data/l4d2panel.sqlite3 MIGRATIONS_DIR=/app/migrations/sqlite DATA_DIR=/app/data SPOOL_DIR=/app/spool
ENTRYPOINT ["/usr/local/bin/l4d2panel"]
