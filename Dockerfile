# syntax=docker/dockerfile:1
# Base images are pinned by digest; Dependabot proposes updates.

# ---- build ----
FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
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
COPY config/config.example.json /etc/personal-ai/config.example.json
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/server"]
