FROM golang:1.23-bookworm AS builder

WORKDIR /app

# Install required packages using apt-get
RUN apt-get update && \
    apt-get install -y \
    git \
    make \
    build-essential \
    libzmq3-dev \
    libczmq-dev \
    libsodium-dev \
    pkg-config \
    gcc \
    libc-dev \
    cmake \
    python3 \
    libssl-dev

# Copy dependency files first for better caching
COPY go.mod ./
COPY go.sum ./

RUN go mod download

# Copy the rest of the source code
COPY . ./

ARG GOFLAGS
# Build only the galexie binary instead of all packages
RUN CGO_ENABLED=0 go build -o galexie 

FROM ubuntu:22.04
ARG STELLAR_CORE_VERSION
ENV STELLAR_CORE_VERSION=${STELLAR_CORE_VERSION:-*}
ENV STELLAR_CORE_BINARY_PATH=/usr/bin/stellar-core

ENV DEBIAN_FRONTEND=noninteractive
# Install required packages and Stellar Core in a single layer
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    wget \
    gnupg \
    apt-utils && \
    wget -qO - https://apt.stellar.org/SDF.asc | APT_KEY_DONT_WARN_ON_DANGEROUS_USAGE=true apt-key add - && \
    echo "deb https://apt.stellar.org focal stable" >/etc/apt/sources.list.d/SDF.list && \
    echo "deb https://apt.stellar.org focal unstable" >/etc/apt/sources.list.d/SDF-unstable.list && \
    apt-get update && \
    apt-get install -y stellar-core=${STELLAR_CORE_VERSION} && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/galexie /usr/local/bin/galexie

ENTRYPOINT ["/usr/local/bin/galexie"]

CMD ["--help"]

