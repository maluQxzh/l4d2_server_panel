# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM node:24-alpine AS web-build
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25.12-alpine AS go-build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
COPY --from=web-build /src/web/dist ./internal/control/static
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X github.com/l4d2-panel/l4d2panel/internal/version.Version=${VERSION} -X github.com/l4d2-panel/l4d2panel/internal/version.Commit=${COMMIT} -X github.com/l4d2-panel/l4d2panel/internal/version.Date=${BUILD_DATE}" \
    -o /out/l4d2panel ./cmd/control

FROM alpine:3.22
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="L4D2 Panel Control" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S panel \
    && adduser -S -D -H -G panel panel
WORKDIR /app
COPY --from=go-build /out/l4d2panel /usr/local/bin/l4d2panel
COPY migrations/sqlite ./migrations/sqlite
COPY docs ./docs
RUN mkdir -p /app/data /app/spool && chown -R panel:panel /app
USER panel:panel
EXPOSE 8080
ENV LISTEN_ADDR=:8080 DATABASE_PATH=/app/data/l4d2panel.sqlite3 MIGRATIONS_DIR=/app/migrations/sqlite DATA_DIR=/app/data SPOOL_DIR=/app/spool
ENTRYPOINT ["/usr/local/bin/l4d2panel"]
