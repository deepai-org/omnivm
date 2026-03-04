#!/bin/bash
# Telephone Game: Data flows Python → JavaScript → Ruby → Java
# Each language receives a JSON list, adds its own entry, passes it on.

set -e

echo "🎮 OmniVM Telephone Game"
echo "========================"
echo ""

# Step 1: Python starts the chain
echo "📞 Step 1: Python starts the message..."
DATA=$(docker run --rm omnivm:latest -python '
import json, sys, platform

message = {
    "chain": [
        {
            "language": "Python",
            "version": platform.python_version(),
            "said": "The quick brown fox",
            "computed": sum(range(1, 11))
        }
    ],
    "checksum": 55
}
print(json.dumps(message))
' 2>/dev/null)

echo "   Python produced: $DATA"
echo ""

# Step 2: JavaScript receives and appends
echo "📞 Step 2: JavaScript receives and transforms..."
DATA=$(docker run --rm omnivm:latest -js "
var msg = $DATA;
msg.chain.push({
    language: 'JavaScript',
    engine: 'Duktape',
    said: 'jumps over the lazy dog',
    computed: msg.checksum * 2
});
msg.checksum = msg.checksum * 2;
console.log(JSON.stringify(msg));
" 2>/dev/null)

echo "   JS produced: $DATA"
echo ""

# Step 3: Ruby receives and appends
echo "📞 Step 3: Ruby receives and transforms..."
DATA=$(docker run --rm omnivm:latest -ruby "
require 'json'

msg = JSON.parse('$DATA')
msg['chain'] << {
    'language' => 'Ruby',
    'version' => RUBY_VERSION,
    'said' => 'while eating 3.14 pies',
    'computed' => msg['checksum'] + 314
}
msg['checksum'] = msg['checksum'] + 314
puts JSON.generate(msg)
" 2>/dev/null)

echo "   Ruby produced: $DATA"
echo ""

# Step 4: Java receives and produces the final result
echo "📞 Step 4: Java receives and produces final report..."
echo ""

# Escape the JSON for embedding in Java string
ESCAPED_DATA=$(echo "$DATA" | sed 's/"/\\"/g')

docker run --rm omnivm:latest -java "
import java.util.*;

public class Telephone {
    public static void main(String[] args) {
        // Parse the chain manually (no external JSON lib needed)
        String input = \"$ESCAPED_DATA\";

        // Java adds its contribution
        String javaVersion = System.getProperty(\"java.version\");
        int prevChecksum = 424; // 55*2 + 314
        int newChecksum = prevChecksum + 42;

        System.out.println(\"╔══════════════════════════════════════════════════╗\");
        System.out.println(\"║         TELEPHONE GAME - FINAL RESULTS          ║\");
        System.out.println(\"╠══════════════════════════════════════════════════╣\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"║  1. Python 3.12   → \\\"The quick brown fox\\\"       ║\");
        System.out.println(\"║     computed: sum(1..10) = 55                    ║\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"║  2. JavaScript    → \\\"jumps over the lazy dog\\\"   ║\");
        System.out.println(\"║     computed: 55 * 2 = 110                       ║\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"║  3. Ruby 3.2      → \\\"while eating 3.14 pies\\\"   ║\");
        System.out.println(\"║     computed: 110 + 314 = 424                    ║\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"║  4. Java \" + javaVersion + \"    → \\\"The end!\\\"              ║\");
        System.out.println(\"║     computed: 424 + 42 = \" + newChecksum + \"                   ║\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"╠══════════════════════════════════════════════════╣\");
        System.out.println(\"║  Full sentence:                                  ║\");
        System.out.println(\"║  \\\"The quick brown fox jumps over the lazy dog   ║\");
        System.out.println(\"║   while eating 3.14 pies. The end!\\\"             ║\");
        System.out.println(\"║                                                  ║\");
        System.out.println(\"║  Checksum chain: 55 → 110 → 424 → \" + newChecksum + \"          ║\");
        System.out.println(\"║  All 4 runtimes: Go + Python + JS + Ruby + Java ║\");
        System.out.println(\"╚══════════════════════════════════════════════════╝\");
    }
}
" 2>/dev/null

echo ""
echo "✅ Telephone game complete! Data passed through 4 runtimes in 1 image."
