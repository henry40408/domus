# syntax=docker/dockerfile:1

# ---- build: cross-compile a static Go binary --------------------------------
# Runs on the native build platform; Go cross-compiles to the target with
# CGO off (the SQLite driver is pure Go), so no qemu and no C toolchain.
# No Go version tag: the image's Go is used when it is at least go.mod's
# `toolchain` line; otherwise GOTOOLCHAIN=auto fetches that line (auto only
# moves up). Keep the `bookworm` suffix: a bare tag resolves to trixie, a
# silent Debian bump.
FROM --platform=$BUILDPLATFORM golang:bookworm AS build

WORKDIR /app

# The official image sets GOTOOLCHAIN=local, which ignores go.mod's toolchain
# line. Fetch a newer toolchain in a layer keyed on go.mod/go.sum alone, so
# source edits do not re-download it.
ENV GOTOOLCHAIN=auto
COPY go.mod go.sum ./
RUN go version

COPY . .

# `vendor/` ships in the repository, so the build needs no network. .git is
# excluded by .dockerignore, so the version comes from the GIT_VERSION build
# arg the CI workflow passes.
ARG TARGETOS
ARG TARGETARCH
ARG GIT_VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=vendor -trimpath \
      -ldflags "-s -w -X github.com/henry40408/domus/internal/version.version=${GIT_VERSION}" \
      -o /out/domus ./cmd/domus

# ---- runtime: minimal static image (CA certs + tzdata, no shell) ------------
# Runs as root (distroless/static default) so a bind-mounted /data stays
# writable without a permissions change.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/domus /domus

VOLUME /data

ENV DOMUS_DATA_DIR=/data
ENV DOMUS_BIND=0.0.0.0:8123

EXPOSE 8123

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/domus", "healthcheck"]

ENTRYPOINT ["/domus"]
