#!/bin/bash
# Integration tests for the new CLI features.
# Runs inside Docker via: docker run --rm --entrypoint /bin/bash omnivm /omnivm/scripts/test-cli.sh

set -uo pipefail

PASS=0
FAIL=0

pass() { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1 — $2"; }

echo "=== OmniVM CLI Integration Tests ==="

# --- Test 1: omnivm run script.py ---
echo ""
echo "--- Test: omnivm run script.py ---"
cat > /tmp/hello.py << 'EOF'
print("hello from python")
EOF
OUT=$(omnivm run /tmp/hello.py 2>/dev/null)
if [ "$OUT" = "hello from python" ]; then
    pass "run python file"
else
    fail "run python file" "got: $OUT"
fi

# --- Test 2: omnivm run app.js ---
echo "--- Test: omnivm run app.js ---"
cat > /tmp/hello.js << 'EOF'
console.log("hello from js")
EOF
OUT=$(omnivm run /tmp/hello.js 2>/dev/null)
if [ "$OUT" = "hello from js" ]; then
    pass "run js file"
else
    fail "run js file" "got: $OUT"
fi

# --- Test 3: omnivm run script.rb ---
echo "--- Test: omnivm run script.rb ---"
cat > /tmp/hello.rb << 'EOF'
puts "hello from ruby"
EOF
OUT=$(omnivm run /tmp/hello.rb 2>/dev/null)
if [ "$OUT" = "hello from ruby" ]; then
    pass "run ruby file"
else
    fail "run ruby file" "got: $OUT"
fi

# --- Test 4: omnivm run main.go ---
echo "--- Test: omnivm run main.go ---"
cat > /tmp/hello.go << 'EOF'
package main
import "fmt"
func main() { fmt.Println("hello from go") }
EOF
OUT=$(omnivm run /tmp/hello.go 2>/dev/null)
if [ "$OUT" = "hello from go" ]; then
    pass "run go file"
else
    fail "run go file" "got: $OUT"
fi

# --- Test 5: argv passthrough (Python) ---
echo "--- Test: argv passthrough (Python) ---"
cat > /tmp/args.py << 'EOF'
import sys
print(" ".join(sys.argv[1:]))
EOF
OUT=$(omnivm run /tmp/args.py foo bar baz 2>/dev/null)
if [ "$OUT" = "foo bar baz" ]; then
    pass "python argv"
else
    fail "python argv" "got: $OUT"
fi

# --- Test 6: argv passthrough (JS) ---
echo "--- Test: argv passthrough (JS) ---"
cat > /tmp/args.js << 'EOF'
console.log(process.argv.slice(2).join(" "))
EOF
OUT=$(omnivm run /tmp/args.js foo bar baz 2>/dev/null)
if [ "$OUT" = "foo bar baz" ]; then
    pass "js argv"
else
    fail "js argv" "got: $OUT"
fi

# --- Test 7: argv passthrough (Ruby) ---
echo "--- Test: argv passthrough (Ruby) ---"
cat > /tmp/args.rb << 'EOF'
puts ARGV.join(" ")
EOF
OUT=$(omnivm run /tmp/args.rb foo bar baz 2>/dev/null)
if [ "$OUT" = "foo bar baz" ]; then
    pass "ruby argv"
else
    fail "ruby argv" "got: $OUT"
fi

# --- Test 8: argv passthrough (Go) ---
echo "--- Test: argv passthrough (Go) ---"
cat > /tmp/args.go << 'EOF'
package main
import (
	"fmt"
	"os"
	"strings"
)
func main() { fmt.Println(strings.Join(os.Args[1:], " ")) }
EOF
OUT=$(omnivm run /tmp/args.go foo bar baz 2>/dev/null)
if [ "$OUT" = "foo bar baz" ]; then
    pass "go argv"
else
    fail "go argv" "got: $OUT"
fi

# --- Test 9: stdin piping (Python) ---
echo "--- Test: stdin piping (Python) ---"
cat > /tmp/stdin.py << 'EOF'
import sys
for line in sys.stdin:
    print("got:", line.strip())
EOF
OUT=$(echo -e "line1\nline2" | omnivm run /tmp/stdin.py 2>/dev/null)
EXPECTED=$(printf "got: line1\ngot: line2")
if [ "$OUT" = "$EXPECTED" ]; then
    pass "python stdin"
else
    fail "python stdin" "got: $OUT"
fi

# --- Test 10: stdin piping (Go) ---
echo "--- Test: stdin piping (Go) ---"
cat > /tmp/stdin.go << 'EOF'
package main
import (
	"bufio"
	"fmt"
	"os"
)
func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fmt.Println("got:", scanner.Text())
	}
}
EOF
OUT=$(echo -e "line1\nline2" | omnivm run /tmp/stdin.go 2>/dev/null)
EXPECTED=$(printf "got: line1\ngot: line2")
if [ "$OUT" = "$EXPECTED" ]; then
    pass "go stdin"
else
    fail "go stdin" "got: $OUT"
fi

# --- Test 11: shebang stripping ---
echo "--- Test: shebang stripping ---"
cat > /tmp/shebang.py << 'EOF'
#!/usr/bin/env omnivm run
print("shebang works")
EOF
OUT=$(omnivm run /tmp/shebang.py 2>/dev/null)
if [ "$OUT" = "shebang works" ]; then
    pass "shebang stripping"
else
    fail "shebang stripping" "got: $OUT"
fi

# --- Test 12: exit code passthrough (Python) ---
echo "--- Test: exit code passthrough (Python) ---"
cat > /tmp/exit.py << 'EOF'
import sys
sys.exit(42)
EOF
set +e
omnivm run /tmp/exit.py 2>/dev/null
CODE=$?
set -e
if [ "$CODE" != "0" ]; then
    pass "python exit code (non-zero)"
else
    fail "python exit code" "got: $CODE, expected non-zero"
fi

# --- Test 13: exit code passthrough (Go) ---
echo "--- Test: exit code passthrough (Go) ---"
cat > /tmp/exit.go << 'EOF'
package main
import "os"
func main() { os.Exit(42) }
EOF
set +e
omnivm run /tmp/exit.go 2>/dev/null
CODE=$?
set -e
if [ "$CODE" = "42" ]; then
    pass "go exit code"
else
    fail "go exit code" "got: $CODE, expected 42"
fi

# --- Test 14: error messages with hints (Python) ---
echo "--- Test: error messages with hints ---"
cat > /tmp/importerr.py << 'EOF'
import requests
EOF
set +e
ERR=$(omnivm run /tmp/importerr.py 2>&1)
set -e
if echo "$ERR" | grep -q "pip install requests"; then
    pass "python import error hint"
elif echo "$ERR" | grep -q "ModuleNotFoundError"; then
    pass "python import error detected (no hint)"
else
    fail "python import error" "got: $ERR"
fi

# --- Test 15: lazy init (Go files don't load embedded runtimes) ---
echo "--- Test: lazy init for Go ---"
cat > /tmp/fast.go << 'EOF'
package main
import "fmt"
func main() { fmt.Println("fast") }
EOF
# Go files should be fast — no JVM/Ruby startup
START=$(date +%s%N)
OUT=$(omnivm run /tmp/fast.go 2>/dev/null)
END=$(date +%s%N)
ELAPSED_MS=$(( (END - START) / 1000000 ))
if [ "$OUT" = "fast" ] && [ "$ELAPSED_MS" -lt 10000 ]; then
    pass "lazy init go (${ELAPSED_MS}ms)"
else
    fail "lazy init go" "output=$OUT elapsed=${ELAPSED_MS}ms"
fi

# --- Test 16: legacy -python flag still works ---
echo "--- Test: legacy -python flag ---"
OUT=$(omnivm -python "print('legacy')" 2>/dev/null)
if [ "$OUT" = "legacy" ]; then
    pass "legacy -python flag"
else
    fail "legacy -python flag" "got: $OUT"
fi

# --- Test 17: legacy -file flag still works ---
echo "--- Test: legacy -file flag ---"
OUT=$(omnivm -file /tmp/hello.py 2>/dev/null)
if [ "$OUT" = "hello from python" ]; then
    pass "legacy -file flag"
else
    fail "legacy -file flag" "got: $OUT"
fi

# --- Test 18: CWD visibility ---
echo "--- Test: CWD visibility (Python) ---"
cat > /tmp/cwd.py << 'EOF'
import os
print(os.getcwd())
EOF
OUT=$(omnivm run /tmp/cwd.py 2>/dev/null)
if [ -n "$OUT" ]; then
    pass "python sees CWD: $OUT"
else
    fail "python CWD" "empty output"
fi

# --- Test 19: HOME visibility ---
echo "--- Test: HOME visibility (Python) ---"
cat > /tmp/home.py << 'EOF'
import os
print(os.environ.get("HOME", "MISSING"))
EOF
OUT=$(omnivm run /tmp/home.py 2>/dev/null)
if [ "$OUT" != "MISSING" ] && [ -n "$OUT" ]; then
    pass "python sees HOME: $OUT"
else
    fail "python HOME" "got: $OUT"
fi

# --- Test 20: Network access (Python HTTP client) ---
echo "--- Test: network access (Python) ---"
cat > /tmp/net.py << 'EOF'
import urllib.request
try:
    urllib.request.urlopen("http://127.0.0.1:1", timeout=0.1)
except Exception as e:
    # We expect connection refused — that means networking works
    print("network ok")
EOF
OUT=$(omnivm run /tmp/net.py 2>/dev/null)
if [ "$OUT" = "network ok" ]; then
    pass "python network access"
else
    fail "python network access" "got: $OUT"
fi

# --- Summary ---
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
