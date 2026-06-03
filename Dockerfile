# OmniVM Dockerfile
# Multi-stage build embedding Python, JavaScript (Node.js/V8), JVM, and Ruby
# into a single Go binary via cgo.
#
# Build: docker build -t omnivm .
# Run:   docker run -it omnivm
# Test:  docker run omnivm -python "print('hello')"
#
# Base: Debian sid — provides Python 3.14, Node.js 22 (libnode127),
# Ruby 3.3, and JDK 21+ from standard repos (no PPAs needed).

# ============================================================
# Stage 1: Build environment with all dev headers
# ============================================================
FROM debian:sid AS builder

ENV DEBIAN_FRONTEND=noninteractive

# Base build tools (binutils-gold needed: Go's linker uses -fuse-ld=gold)
RUN apt-get update && apt-get install -y \
    build-essential \
    binutils-gold \
    pkg-config \
    curl \
    xz-utils \
    && rm -rf /var/lib/apt/lists/*

# ---- Go (architecture-aware) ----
ENV GO_VERSION=1.23.6
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

# ---- Python 3.14 dev ----
RUN apt-get update && apt-get install -y python3.14-dev python3.14-venv && rm -rf /var/lib/apt/lists/* && \
    update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.14 1 && \
    ln -sf /usr/bin/python3.14 /usr/local/bin/python3

# ---- Ruby dev ----
RUN apt-get update && apt-get install -y ruby-dev ruby-nokogiri ruby-rack libsqlite3-dev && rm -rf /var/lib/apt/lists/*
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("nokogiri"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "nokogiri.rb"), File.join(site, "nokogiri.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "nokogiri"), File.join(site, "nokogiri")); FileUtils.ln_sf(File.join(spec.extension_dir, "nokogiri", "nokogiri.so"), File.join(site, "nokogiri", "nokogiri.so"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("rack"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "rack.rb"), File.join(site, "rack.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "rack"), File.join(site, "rack"))'
RUN gem install activerecord sqlite3 --no-document
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("concurrent-ruby"); site = RbConfig::CONFIG["sitedir"]; lib = File.join(spec.full_gem_path, "lib", "concurrent-ruby"); FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(lib, "concurrent.rb"), File.join(site, "concurrent.rb")); FileUtils.ln_sf(File.join(lib, "concurrent"), File.join(site, "concurrent"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("i18n"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "i18n.rb"), File.join(site, "i18n.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "i18n"), File.join(site, "i18n"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("tzinfo"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "tzinfo.rb"), File.join(site, "tzinfo.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "tzinfo"), File.join(site, "tzinfo"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activesupport"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_support.rb"), File.join(site, "active_support.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_support"), File.join(site, "active_support"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activemodel"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_model.rb"), File.join(site, "active_model.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_model"), File.join(site, "active_model"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activerecord"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_record.rb"), File.join(site, "active_record.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_record"), File.join(site, "active_record")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "arel.rb"), File.join(site, "arel.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "arel"), File.join(site, "arel"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("sqlite3"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "sqlite3.rb"), File.join(site, "sqlite3.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "sqlite3"), File.join(site, "sqlite3")); native = Dir[File.join(spec.extension_dir, "**", "*.so")].first; if native; FileUtils.mkdir_p(File.join(site, "sqlite3")); FileUtils.ln_sf(native, File.join(site, "sqlite3", File.basename(native))); end'

# ---- JDK (full — needed for javax.tools.JavaCompiler) ----
RUN apt-get update && apt-get install -y default-jdk && rm -rf /var/lib/apt/lists/*
ENV JAVA_HOME=/usr/lib/jvm/default-java
ENV LD_PRELOAD=/usr/lib/jvm/default-java/lib/libjsig.so

# ---- Compile OmniVMRunner.java and OmniVM.java (JVM helpers) ----
COPY runtime/java/ /tmp/java-src/
RUN mkdir -p /omnivm/java && \
    javac -d /omnivm/java /tmp/java-src/OmniVMRunner.java /tmp/java-src/OmniVM.java && \
    echo "Java helpers compiled OK"

# ---- Node.js 22 (shared library for JS embedding) ----
RUN apt-get update && apt-get install -y \
    libnode-dev \
    nodejs \
    npm \
    && rm -rf /var/lib/apt/lists/*

# Framework dependencies used by manifest-runner examples and cross-repo tests.
# Debian disables ensurepip for system Python, so keep Python packages isolated
# while exposing them to the embedded interpreter via PYTHONPATH.
RUN python3.14 -m venv /opt/omnivm-python && \
    /opt/omnivm-python/bin/pip install --no-cache-dir \
      "Django>=5,<6" \
      pandas \
      numpy \
      Pillow \
      polars \
      fastapi \
      Flask \
      beautifulsoup4 \
      pydantic \
      SQLAlchemy \
      Jinja2 \
      Markdown \
      httpx \
      aiohttp \
      requests
ENV PYTHONPATH="/opt/omnivm-python/lib/python3.14/site-packages:${PYTHONPATH}"
RUN cd /usr/local/lib && npm install \
      express \
      zod \
      cheerio \
      lodash \
      d3-shape@2 \
      marked@4 \
      react \
      react-dom \
      undici \
      2>&1 | tail -1
ENV NODE_PATH=/usr/local/lib/node_modules

# Build the Node.js + V8 bridge shim as a shared library
# libnode.so is in /usr/lib/<arch>/, headers in /usr/include/node/
COPY scripts/v8_bridge_node.cc /tmp/v8_bridge_node.cc
COPY pkg/javascript/v8_bridge.h /tmp/v8_bridge.h
RUN LIBNODE_DIR=$(dirname $(find /usr/lib -name "libnode.so" -print -quit)) && \
    g++ -shared -fPIC -std=c++20 -o /usr/local/lib/libv8.so \
        /tmp/v8_bridge_node.cc \
        -I/usr/include/node \
        -I/tmp \
        -L${LIBNODE_DIR} -lnode \
        -Wl,-rpath,${LIBNODE_DIR} && \
    ln -sf /usr/local/lib/libv8.so /usr/local/lib/libv8_libplatform.so && \
    ln -sf /usr/local/lib/libv8.so /usr/local/lib/libv8_libbase.so && \
    ldconfig

# ---- Copy source (ordered by change frequency for cache efficiency) ----
WORKDIR /build

# 1. go.mod + dep download (almost never changes)
COPY go.mod ./
RUN go mod download

# 2. Scripts + build infrastructure (rarely changes)
COPY scripts/ scripts/

# 3. Source code (frequently changes — cache-bust point for build)
COPY pkg/ pkg/
COPY cmd/ cmd/
COPY pyomnivm/ pyomnivm/
COPY integration_test.go ./
RUN chmod +x scripts/python3-polyscript && \
    ln -sf /build/scripts/python3-polyscript /usr/local/bin/python3-polyscript

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

# Create libs directory for user JARs and bundled CLI-test dependencies.
ARG GSON_VERSION=2.10.1
ARG COMMONS_CSV_VERSION=1.10.0
ARG JSOUP_VERSION=1.17.2
ARG OKHTTP_VERSION=3.14.9
ARG OKIO_VERSION=1.17.5
ARG JACKSON_VERSION=2.17.2
ARG REACTOR_VERSION=3.6.6
ARG REACTIVE_STREAMS_VERSION=1.0.4
ARG RXJAVA_VERSION=3.1.10
RUN mkdir -p /omnivm/libs && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/google/code/gson/gson/${GSON_VERSION}/gson-${GSON_VERSION}.jar" \
        -o "/omnivm/libs/gson-${GSON_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/org/apache/commons/commons-csv/${COMMONS_CSV_VERSION}/commons-csv-${COMMONS_CSV_VERSION}.jar" \
        -o "/omnivm/libs/commons-csv-${COMMONS_CSV_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/org/jsoup/jsoup/${JSOUP_VERSION}/jsoup-${JSOUP_VERSION}.jar" \
        -o "/omnivm/libs/jsoup-${JSOUP_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/squareup/okhttp3/okhttp/${OKHTTP_VERSION}/okhttp-${OKHTTP_VERSION}.jar" \
        -o "/omnivm/libs/okhttp-${OKHTTP_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/squareup/okio/okio/${OKIO_VERSION}/okio-${OKIO_VERSION}.jar" \
        -o "/omnivm/libs/okio-${OKIO_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/fasterxml/jackson/core/jackson-databind/${JACKSON_VERSION}/jackson-databind-${JACKSON_VERSION}.jar" \
        -o "/omnivm/libs/jackson-databind-${JACKSON_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/fasterxml/jackson/core/jackson-core/${JACKSON_VERSION}/jackson-core-${JACKSON_VERSION}.jar" \
        -o "/omnivm/libs/jackson-core-${JACKSON_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/com/fasterxml/jackson/core/jackson-annotations/${JACKSON_VERSION}/jackson-annotations-${JACKSON_VERSION}.jar" \
        -o "/omnivm/libs/jackson-annotations-${JACKSON_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/io/projectreactor/reactor-core/${REACTOR_VERSION}/reactor-core-${REACTOR_VERSION}.jar" \
        -o "/omnivm/libs/reactor-core-${REACTOR_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/org/reactivestreams/reactive-streams/${REACTIVE_STREAMS_VERSION}/reactive-streams-${REACTIVE_STREAMS_VERSION}.jar" \
        -o "/omnivm/libs/reactive-streams-${REACTIVE_STREAMS_VERSION}.jar" && \
    curl -fsSL \
        "https://repo1.maven.org/maven2/io/reactivex/rxjava3/rxjava/${RXJAVA_VERSION}/rxjava-${RXJAVA_VERSION}.jar" \
        -o "/omnivm/libs/rxjava-${RXJAVA_VERSION}.jar"

# 5. Examples AFTER build (most frequent changes, no rebuild needed)
COPY examples/ examples/

# Keep the builder stage runnable for local Docker targets. This avoids
# requiring BuildKit to skip the tester stage when a quick image is needed.
RUN mkdir -p /omnivm/scripts && \
    cp -R examples /omnivm/examples && \
    cp scripts/test-cli.sh /omnivm/scripts/test-cli.sh && \
    mkdir -p /var/data && \
    touch /var/data/app.log /var/data/error.log /var/data/debug.log \
          /var/data/server.log /var/data/config.yaml /var/data/readme.txt \
          /var/data/Thumbs.db /var/data/.DS_Store
ENTRYPOINT ["omnivm"]

# ============================================================
# Stage 2: Run tests inside Docker
# ============================================================
FROM builder AS tester

WORKDIR /build

# Pure Go tests (race detector enabled)
RUN go test -race -v ./pkg/cli/ ./pkg/dispatcher/ ./pkg/errmsg/ ./pkg/omnivm/ ./pkg/signals/ ./pkg/arrow/ ./pkg/handles/ ./pkg/watchdog/ ./pkg/manifest/

# Go plugin tests — cannot use -race because the test binary and dynamically
# compiled plugins must share identical runtime/internal/sys instrumentation.
RUN go test -v -count=1 ./pkg/golang/

# cgo-linked runtime tests
RUN LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit) && \
    export LD_LIBRARY_PATH="${LIBJVM_DIR}:/usr/local/lib:${LD_LIBRARY_PATH}" && \
    go test -v -count=1 ./pkg/polyglot/ 2>&1 && \
    go test -v -count=1 ./pkg/python/ 2>&1 && \
    go test -v -count=1 ./pkg/javascript/ 2>&1 && \
    go test -v -count=1 ./pkg/ruby/ 2>&1 && \
    go test -v -count=1 ./pkg/engine/ 2>&1 && \
    echo "Runtime tests completed"

# Integration tests (cross-runtime, requires all runtimes initialized)
RUN LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit) && \
    export LD_LIBRARY_PATH="${LIBJVM_DIR}:/usr/local/lib:${LD_LIBRARY_PATH}" && \
    go test -v -count=1 -tags=integration . 2>&1 && \
    echo "Integration tests completed"

# Python package unit tests (pyomnivm — pure Python, no libomnivm.so needed)
RUN python3 -m unittest discover -s pyomnivm -p 'test_*.py' -v

# ============================================================
# Stage 3: Runtime image (full JDK for javax.tools.JavaCompiler)
# ============================================================
FROM debian:sid AS runtime

ENV DEBIAN_FRONTEND=noninteractive

# Full JDK needed for in-memory Java compilation
# nodejs pulls libnode (libnode127 or libnode137 depending on sid version)
RUN apt-get update && apt-get install -y \
    python3.14 \
    python3.14-dev \
    python3.14-venv \
    ruby \
    ruby-nokogiri \
    ruby-rack \
    libruby \
    default-jdk \
    nodejs \
    npm \
    build-essential \
    binutils-gold \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/* && \
    update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.14 1 && \
    ln -sf /usr/bin/python3.14 /usr/local/bin/python3
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("rack"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "rack.rb"), File.join(site, "rack.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "rack"), File.join(site, "rack"))'
RUN gem install activerecord sqlite3 --no-document
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("concurrent-ruby"); site = RbConfig::CONFIG["sitedir"]; lib = File.join(spec.full_gem_path, "lib", "concurrent-ruby"); FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(lib, "concurrent.rb"), File.join(site, "concurrent.rb")); FileUtils.ln_sf(File.join(lib, "concurrent"), File.join(site, "concurrent"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("i18n"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "i18n.rb"), File.join(site, "i18n.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "i18n"), File.join(site, "i18n"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("tzinfo"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "tzinfo.rb"), File.join(site, "tzinfo.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "tzinfo"), File.join(site, "tzinfo"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activesupport"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_support.rb"), File.join(site, "active_support.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_support"), File.join(site, "active_support"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activemodel"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_model.rb"), File.join(site, "active_model.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_model"), File.join(site, "active_model"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("activerecord"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_record.rb"), File.join(site, "active_record.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "active_record"), File.join(site, "active_record")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "arel.rb"), File.join(site, "arel.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "arel"), File.join(site, "arel"))'
RUN ruby -rfileutils -e 'spec = Gem::Specification.find_by_name("sqlite3"); site = RbConfig::CONFIG["sitedir"]; FileUtils.mkdir_p(site); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "sqlite3.rb"), File.join(site, "sqlite3.rb")); FileUtils.ln_sf(File.join(spec.full_gem_path, "lib", "sqlite3"), File.join(site, "sqlite3")); native = Dir[File.join(spec.extension_dir, "**", "*.so")].first; if native; FileUtils.mkdir_p(File.join(site, "sqlite3")); FileUtils.ln_sf(native, File.join(site, "sqlite3", File.basename(native))); end'

# Copy Go toolchain from builder (needed for Go plugin compilation at runtime)
COPY --from=builder /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Copy V8 bridge shim (rarely changes — libnode.so comes from apt-installed libnode127)
COPY --from=builder /usr/local/lib/libv8.so /usr/local/lib/
COPY --from=builder /usr/local/lib/libv8_libplatform.so /usr/local/lib/
COPY --from=builder /usr/local/lib/libv8_libbase.so /usr/local/lib/
RUN ldconfig

# Python ecosystem libraries used by manifest examples.
COPY --from=builder /opt/omnivm-python /opt/omnivm-python
ENV PYTHONPATH="/opt/omnivm-python/lib/python3.14/site-packages:${PYTHONPATH}"

# JavaScript ecosystem libraries used by manifest examples.
RUN cd /usr/local/lib && npm install \
      express \
      zod \
      cheerio \
      lodash \
      d3-shape@2 \
      marked@4 \
      react \
      react-dom \
      undici \
      2>&1 | tail -1
ENV NODE_PATH=/usr/local/lib/node_modules

# Ensure libjvm is findable at runtime
RUN LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit 2>/dev/null) && \
    if [ -n "$LIBJVM_DIR" ]; then \
        echo "$LIBJVM_DIR" > /etc/ld.so.conf.d/jvm.conf && ldconfig; \
    fi

# JVM signal chaining: libjsig.so intercepts signal()/sigaction() calls
# so the JVM's SIGSEGV handler (used for NullPointerException) chains
# properly with Ruby's and Go's signal handlers instead of conflicting.
RUN LIBJSIG=$(find /usr/lib/jvm -name "libjsig.so" -print -quit 2>/dev/null) && \
    if [ -n "$LIBJSIG" ]; then echo "LD_PRELOAD=$LIBJSIG" >> /etc/environment; fi
ENV LD_PRELOAD=/usr/lib/jvm/default-java/lib/libjsig.so

# Copy bundled Java libraries used by examples and CLI integration tests.
COPY --from=builder /omnivm/libs/ /omnivm/libs/

ENV GOMAXPROCS=4
ENV JAVA_HOME=/usr/lib/jvm/default-java

# Copy the compiled OmniVMRunner class (rarely changes)
COPY --from=builder /omnivm/java/ /omnivm/java/

# Copy the Go binaries (change most often — keep last)
COPY --from=builder /usr/local/bin/omnivm /usr/local/bin/omnivm
COPY --from=builder /usr/local/bin/telephone /usr/local/bin/telephone
COPY --from=builder /usr/local/bin/stresstest /usr/local/bin/stresstest
COPY --from=builder /usr/local/bin/express-demo /usr/local/bin/express-demo
COPY --from=builder /usr/local/bin/manifest-runner /usr/local/bin/manifest-runner

# Python interpreter mode: symlink python3 → omnivm so that
# "python3 -m pytest", "pip install", "gunicorn" all work transparently.
# When invoked as python3, omnivm delegates to Py_BytesMain().
RUN ln -sf /usr/local/bin/omnivm /usr/local/bin/python3-omnivm
# Note: We use python3-omnivm rather than overwriting python3 since the
# runtime image still needs system python3 for apt/pip. Users building
# their own image (FROM omnivm/python:3.14-slim) would set:
#   RUN ln -sf /usr/local/bin/omnivm /usr/local/bin/python3

# PolyScript Python mode keeps the process as stock CPython. That makes it
# safe for Passenger/Gunicorn prefork modes because the Go runtime is not
# loaded until explicit post-fork OmniVM initialization or an external
# manifest-runner process is used.
COPY --from=builder /build/scripts/python3-polyscript /usr/local/bin/python3-polyscript
RUN chmod +x /usr/local/bin/python3-polyscript
ENV POLYSCRIPT_PYTHON_BIN=python3.14

# libomnivm.so — c-shared library for pip-installable Python package.
# Loaded via ctypes.CDLL post-fork in Gunicorn workers.
COPY --from=builder /usr/local/lib/libomnivm.so /usr/local/lib/libomnivm.so
RUN ldconfig

# Install the omnivm Python package (pure Python, lazy-loads libomnivm.so)
COPY --from=builder /build/pyomnivm/omnivm/ /usr/local/lib/python3.14/dist-packages/omnivm/
COPY --from=builder /build/pyomnivm/polyscript/ /usr/local/lib/python3.14/dist-packages/polyscript/
COPY --from=builder /build/pyomnivm/sitecustomize.py /usr/local/lib/python3.14/dist-packages/sitecustomize.py

# Test data for manifest demos
RUN mkdir -p /var/data && \
    touch /var/data/app.log /var/data/error.log /var/data/debug.log \
          /var/data/server.log /var/data/config.yaml /var/data/readme.txt \
          /var/data/Thumbs.db /var/data/.DS_Store

# Copy example manifests (change most often — keep last)
COPY --from=builder /build/examples/ /omnivm/examples/

# Copy test scripts
COPY --from=builder /build/scripts/test-cli.sh /omnivm/scripts/test-cli.sh

ENTRYPOINT ["omnivm"]
