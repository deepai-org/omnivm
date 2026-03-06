# OmniVM Dockerfile
# Multi-stage build embedding Python, JavaScript (Node.js/V8), JVM, and Ruby
# into a single Go binary via cgo.
#
# Build: docker build -t omnivm .
# Run:   docker run -it omnivm
# Test:  docker run omnivm -python "print('hello')"

# ============================================================
# Stage 1: Build environment with all dev headers
# ============================================================
FROM ubuntu:24.04 AS builder

ENV DEBIAN_FRONTEND=noninteractive

# Base build tools
RUN apt-get update && apt-get install -y \
    build-essential \
    pkg-config \
    curl \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

# ---- Go (architecture-aware) ----
ENV GO_VERSION=1.22.5
RUN ARCH=$(dpkg --print-architecture) && \
    case "$ARCH" in \
      amd64) GOARCH=amd64 ;; \
      arm64) GOARCH=arm64 ;; \
      *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${GOARCH}.tar.gz" | tar -C /usr/local -xz
ENV PATH="/usr/local/go/bin:/go/bin:${PATH}"
ENV GOPATH=/go
ENV GOFLAGS="-buildvcs=false"

# ---- Python 3 dev ----
RUN apt-get update && apt-get install -y python3-dev && rm -rf /var/lib/apt/lists/*

# ---- Ruby dev ----
RUN apt-get update && apt-get install -y ruby-dev && rm -rf /var/lib/apt/lists/*

# ---- JDK (full — needed for javax.tools.JavaCompiler) ----
RUN apt-get update && apt-get install -y default-jdk && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/usr/lib/jvm/default-java

# ---- Compile OmniVMRunner.java and OmniVM.java (JVM helpers) ----
COPY runtime/java/ /tmp/java-src/
RUN mkdir -p /omnivm/java && \
    javac -d /omnivm/java /tmp/java-src/OmniVMRunner.java /tmp/java-src/OmniVM.java && \
    echo "Java helpers compiled OK"

# ---- Node.js (shared library for JS embedding) ----
RUN apt-get update && apt-get install -y \
    libnode-dev \
    nodejs \
    npm \
    && rm -rf /var/lib/apt/lists/*

# Build the Node.js + V8 bridge shim as a shared library
# libnode.so is in /usr/lib/<arch>/, headers in /usr/include/node/
COPY scripts/v8_bridge_node.cc /tmp/v8_bridge_node.cc
COPY pkg/javascript/v8_bridge.h /tmp/v8_bridge.h
RUN LIBNODE_DIR=$(dirname $(find /usr/lib -name "libnode.so" -print -quit)) && \
    g++ -shared -fPIC -std=c++17 -o /usr/local/lib/libv8.so \
        /tmp/v8_bridge_node.cc \
        -I/usr/include/node \
        -I/tmp \
        -L${LIBNODE_DIR} -lnode \
        -Wl,-rpath,${LIBNODE_DIR} && \
    ln -sf /usr/local/lib/libv8.so /usr/local/lib/libv8_libplatform.so && \
    ln -sf /usr/local/lib/libv8.so /usr/local/lib/libv8_libbase.so && \
    ldconfig

# ---- Copy source ----
WORKDIR /build
COPY go.mod ./
COPY pkg/ pkg/
COPY cmd/ cmd/
COPY scripts/ scripts/
COPY examples/ examples/
COPY integration_test.go ./

# ---- Prepare Docker-specific source files ----
# Replace the JS package with the Docker-compatible version (no C++)
RUN cp scripts/javascript_docker.go pkg/javascript/javascript.go && \
    rm -f pkg/javascript/v8_bridge.cc

# Replace the JVM package with the Docker-compatible version (uses OmniVMRunner)
RUN cp scripts/jvm_docker.go pkg/jvm/jvm.go

# Copy the bridge header to v8_bridge.h directory (for include path)
RUN mkdir -p pkg/polyglot

# ---- Build Go binaries ----
RUN chmod +x scripts/build.sh && scripts/build.sh

# Create libs directory for user JARs
RUN mkdir -p /omnivm/libs

# ============================================================
# Stage 2: Run tests inside Docker
# ============================================================
FROM builder AS tester

WORKDIR /build

# Pure Go tests
RUN go test -race -v ./pkg/dispatcher/ ./pkg/signals/ ./pkg/arrow/

# cgo-linked runtime tests
RUN LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit) && \
    export LD_LIBRARY_PATH="${LIBJVM_DIR}:/usr/local/lib:${LD_LIBRARY_PATH}" && \
    go test -v -count=1 ./pkg/python/ 2>&1 && \
    go test -v -count=1 ./pkg/javascript/ 2>&1 && \
    go test -v -count=1 ./pkg/ruby/ 2>&1; \
    echo "Runtime tests completed"

# ============================================================
# Stage 3: Runtime image (full JDK for javax.tools.JavaCompiler)
# ============================================================
FROM ubuntu:24.04 AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Full JDK needed for in-memory Java compilation
# libnode109 provides libnode.so for the V8 bridge shim at runtime
RUN apt-get update && apt-get install -y \
    python3 \
    python3-dev \
    ruby \
    libruby \
    default-jdk \
    libnode109 \
    nodejs \
    npm \
    && rm -rf /var/lib/apt/lists/*

# Copy Go toolchain from builder (needed for Go plugin compilation at runtime)
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Copy the Go binaries
COPY --from=builder /usr/local/bin/omnivm /usr/local/bin/omnivm
COPY --from=builder /usr/local/bin/telephone /usr/local/bin/telephone
COPY --from=builder /usr/local/bin/stresstest /usr/local/bin/stresstest
COPY --from=builder /usr/local/bin/express-demo /usr/local/bin/express-demo
COPY --from=builder /usr/local/bin/manifest-runner /usr/local/bin/manifest-runner

# Copy the compiled OmniVMRunner class
COPY --from=builder /omnivm/java/ /omnivm/java/

# Copy example manifests
COPY --from=builder /build/examples/ /omnivm/examples/

# Create libs directory for user JARs (mount or COPY your .jars here)
RUN mkdir -p /omnivm/libs

# Install Express.js for the express-demo
RUN cd /usr/local/lib && npm install express 2>&1 | tail -1
ENV NODE_PATH=/usr/local/lib/node_modules

# Copy V8 bridge shim (libnode.so comes from apt-installed libnode109)
COPY --from=builder /usr/local/lib/libv8.so /usr/local/lib/
COPY --from=builder /usr/local/lib/libv8_libplatform.so /usr/local/lib/
COPY --from=builder /usr/local/lib/libv8_libbase.so /usr/local/lib/
RUN ldconfig

# Ensure libjvm is findable at runtime
RUN LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit 2>/dev/null) && \
    if [ -n "$LIBJVM_DIR" ]; then \
        echo "$LIBJVM_DIR" > /etc/ld.so.conf.d/jvm.conf && ldconfig; \
    fi

ENV GOMAXPROCS=1
ENV JAVA_HOME=/usr/lib/jvm/default-java

# JVM signal chaining: libjsig.so intercepts signal()/sigaction() calls
# so the JVM's SIGSEGV handler (used for NullPointerException) chains
# properly with Ruby's and Go's signal handlers instead of conflicting.
RUN LIBJSIG=$(find /usr/lib/jvm -name "libjsig.so" -print -quit 2>/dev/null) && \
    if [ -n "$LIBJSIG" ]; then echo "LD_PRELOAD=$LIBJSIG" >> /etc/environment; fi
ENV LD_PRELOAD=/usr/lib/jvm/default-java/lib/libjsig.so

ENTRYPOINT ["omnivm"]
