FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS updatesign
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/updatesign ./cmd/updatesign

FROM rust:1.96.0-slim-bookworm@sha256:bef0c0c8eee5b866fadb56bd83cdf4681921c3851f2b01dc13b621653a06a4f5 AS rust_toolchain

FROM ubuntu@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517

ARG RUST_VERSION=1.96.0
ARG UBUNTU_SNAPSHOT=20260801T000000Z
ENV DEBIAN_FRONTEND=noninteractive \
    RUSTUP_HOME=/opt/rustup \
    CARGO_HOME=/opt/cargo \
    PATH=/opt/cargo/bin:${PATH}

COPY --from=updatesign /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN sed -i "s#http://archive.ubuntu.com/ubuntu/#https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/#; s#http://security.ubuntu.com/ubuntu/#https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/#" /etc/apt/sources.list.d/ubuntu.sources \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      build-essential ca-certificates curl file git libgtk-3-dev \
      libwebkit2gtk-4.1-dev libssl-dev librsvg2-dev nodejs npm pkg-config \
 && rm -rf /var/lib/apt/lists/*

COPY --from=rust_toolchain /usr/local/rustup /opt/rustup
COPY --from=rust_toolchain /usr/local/cargo /opt/cargo
RUN rustup target add x86_64-unknown-linux-gnu

COPY --from=updatesign /out/updatesign /usr/local/bin/updatesign
WORKDIR /work
