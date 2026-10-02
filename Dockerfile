# syntax=docker/dockerfile:1

# ---- build: cross-compile a static musl binary with cargo-zigbuild ----------
# The builder is pinned to the native build platform; zig cross-compiles to the
# target arch's musl triple, so no qemu emulation is needed. The C dependencies
# (bundled SQLite, aws-lc-sys via rustls) are compiled by zig cc.
FROM --platform=$BUILDPLATFORM rust:bookworm AS build

RUN apt-get update \
    && apt-get install -y --no-install-recommends curl xz-utils cmake \
    && rm -rf /var/lib/apt/lists/*

ARG ZIG_VERSION=0.14.1
ARG ZIGBUILD_VERSION=0.23.0
RUN cargo install cargo-zigbuild --version "${ZIGBUILD_VERSION}" --locked
RUN set -eux; \
    case "$(uname -m)" in \
      x86_64) zarch=x86_64 ;; \
      aarch64) zarch=aarch64 ;; \
      *) echo "unsupported build arch $(uname -m)" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://ziglang.org/download/${ZIG_VERSION}/zig-${zarch}-linux-${ZIG_VERSION}.tar.xz" \
      | tar -xJ -C /opt; \
    ln -s "/opt/zig-${zarch}-linux-${ZIG_VERSION}/zig" /usr/local/bin/zig

WORKDIR /app
COPY . .

ARG TARGETARCH
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/app/target,sharing=locked \
    set -eux; \
    case "$TARGETARCH" in \
      amd64) target=x86_64-unknown-linux-musl ;; \
      arm64) target=aarch64-unknown-linux-musl ;; \
      *) echo "unsupported target arch $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    rustup target add "$target"; \
    cargo zigbuild --release --locked --target "$target"; \
    install -Dm755 "target/${target}/release/domus" /out/domus

# ---- runtime: minimal static image (CA certs + tzdata, no shell) ------------
# Runs as root (distroless/static default) so a bind-mounted /data stays
# writable without a permissions change.
FROM gcr.io/distroless/static-debian12

COPY --from=build /out/domus /domus

VOLUME /data

ENV DOMUS_DATA_DIR=/data
ENV DOMUS_BIND=0.0.0.0:8123

EXPOSE 8123

ENTRYPOINT ["/domus"]
