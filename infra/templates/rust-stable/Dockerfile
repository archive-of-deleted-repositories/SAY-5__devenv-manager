# rust-stable template — Rust stable with cargo and sccache preinstalled.
#
# Build args:
#   SCCACHE_VERSION  sccache release tag (default: 0.8.2)
ARG SCCACHE_VERSION=0.8.2

FROM rust:1-slim

ARG SCCACHE_VERSION

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential pkg-config libssl-dev git ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# Install sccache so subsequent `cargo build` runs warm a shared cache
# across sessions when the operator mounts ~/.cache as a named volume.
RUN curl -sSL "https://github.com/mozilla/sccache/releases/download/v${SCCACHE_VERSION}/sccache-v${SCCACHE_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
    | tar -xz -C /usr/local/bin --strip-components=1 "sccache-v${SCCACHE_VERSION}-x86_64-unknown-linux-musl/sccache" \
 && chmod +x /usr/local/bin/sccache

ENV RUSTC_WRAPPER=sccache

WORKDIR /workspace

CMD ["bash"]
