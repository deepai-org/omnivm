#!/bin/bash
set -ex

# Find Java headers
JAVA_INC=$(find /usr/lib/jvm -name "jni.h" -printf "%h" -quit)
JAVA_INC_LINUX="${JAVA_INC}/linux"

# Find libjvm
LIBJVM_DIR=$(find /usr/lib/jvm -name "libjvm.so" -printf "%h" -quit)

echo "Java include: ${JAVA_INC}"
echo "Java lib: ${LIBJVM_DIR}"

export CGO_ENABLED=1

# Create a CGO override file for JVM
cat > /build/pkg/jvm/cgo_flags.go << GOEOF
package jvm

// #cgo CFLAGS: -I${JAVA_INC} -I${JAVA_INC_LINUX}
// #cgo LDFLAGS: -L${LIBJVM_DIR} -ljvm -Wl,-rpath,${LIBJVM_DIR}
import "C"
GOEOF

# Create CGO override for JavaScript (Node.js bridge shim)
# Headers are from libnode-dev (/usr/include/node), bridge shim is libv8.so
cat > /build/pkg/javascript/cgo_flags.go << GOEOF
package javascript

// #cgo CFLAGS: -I/usr/include/node
// #cgo LDFLAGS: -L/usr/local/lib -lv8 -Wl,-rpath,/usr/local/lib
import "C"
GOEOF

cd /build
go build -v -o /usr/local/bin/omnivm ./cmd/omnivm/
go build -v -o /usr/local/bin/telephone ./cmd/telephone/
go build -v -o /usr/local/bin/stresstest ./cmd/stresstest/
go build -v -o /usr/local/bin/express-demo ./cmd/express-demo/
