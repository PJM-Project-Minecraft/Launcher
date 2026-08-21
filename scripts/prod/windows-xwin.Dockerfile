FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS updatesign
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/updatesign ./cmd/updatesign

FROM ubuntu@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517

ARG RUST_VERSION=1.96.0
ARG CARGO_XWIN_VERSION=0.23.1
ENV DEBIAN_FRONTEND=noninteractive \
    RUSTUP_HOME=/opt/rustup \
    CARGO_HOME=/opt/cargo \
    PATH=/opt/cargo/bin:/usr/lib/llvm-18/bin:${PATH}

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential ca-certificates clang-18 curl git lld-18 llvm-18 nodejs npm \
      pkg-config xz-utils \
 && ln -s /usr/bin/clang-cl-18 /usr/local/bin/clang-cl \
 && ln -s /usr/bin/lld-link-18 /usr/local/bin/lld-link \
 && ln -s /usr/bin/llvm-lib-18 /usr/local/bin/llvm-lib \
 && ln -s /usr/bin/llvm-rc-18 /usr/local/bin/llvm-rc \
 && rm -rf /var/lib/apt/lists/*

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
  | sh -s -- -y --profile minimal --default-toolchain "${RUST_VERSION}" \
 && cargo install cargo-xwin --version "${CARGO_XWIN_VERSION}" --locked

RUN rustup target add x86_64-pc-windows-msvc

COPY --from=updatesign /out/updatesign /usr/local/bin/updatesign
WORKDIR /work
