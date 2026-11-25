# syntax=docker/dockerfile:1
# Base images are pinned by digest; Dependabot proposes updates.

# ---- build ----
FROM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/telegram ./cmd/telegram && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/personal-ai ./cmd/cli

# ---- runtime ----
# One image, three executables: the Kubernetes manifests pick one with
# `command`. Distroless static: no shell, CA certificates included, non-root.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG VERSION=dev
LABEL org.opencontainers.image.title="personal-ai" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/ /usr/local/bin/
COPY config/config.example.yaml /etc/personal-ai/config.example.yaml
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
