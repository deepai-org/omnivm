// Telephone Game: Data flows Python → JavaScript → Ruby → Java
// All four runtimes execute on the same Golden Thread in one process.
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
)

func init() {
	runtime.LockOSThread()
}

func must(r pkg.Runtime, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] init failed: %v\n", r.Name(), err)
		os.Exit(1)
	}
}

func run(r pkg.Runtime, code string) string {
	result := r.Execute(code)
	if result.Err != nil {
		fmt.Fprintf(os.Stderr, "[%s] error: %v\n", r.Name(), result.Err)
		os.Exit(1)
	}
	return strings.TrimRight(result.Output, "\n")
}

func main() {
	py := python.New()
	js := javascript.New()
	rb := ruby.New()
	jv := jvm.New()

	must(py, py.Initialize())
	must(js, js.Initialize())
	must(rb, rb.Initialize())
	must(jv, jv.Initialize())

	fmt.Println("=== OmniVM Telephone Game ===")
	fmt.Println("One process. One thread. Four runtimes.\n")

	// Step 1: Python creates the initial data
	fmt.Println("[Python] Starting the chain...")
	data := run(py, `
import json
chain = ["Python: The quick brown fox (sum 1..10 = " + str(sum(range(1,11))) + ")"]
print(json.dumps(chain))
`)
	fmt.Printf("  -> %s\n\n", data)

	// Step 2: JavaScript receives, appends, passes on
	fmt.Println("[JavaScript] Receiving and appending...")
	data = run(js, fmt.Sprintf(`
var chain = %s;
chain.push("JavaScript: jumps over " + (6 * 7) + " lazy dogs");
console.log(JSON.stringify(chain));
`, data))
	fmt.Printf("  -> %s\n\n", data)

	// Step 3: Ruby receives, appends, passes on
	fmt.Println("[Ruby] Receiving and appending...")
	data = run(rb, fmt.Sprintf(`
require 'json'
chain = JSON.parse('%s')
chain << "Ruby: while eating #{Math::PI.round(4)} pies (#{RUBY_VERSION})"
puts JSON.generate(chain)
`, strings.ReplaceAll(data, "'", "\\'")))
	fmt.Printf("  -> %s\n\n", data)

	// Step 4: Java receives, appends, prints final report
	fmt.Println("[Java] Receiving and producing final report...")
	escaped := strings.ReplaceAll(data, `"`, `\"`)
	final := run(jv, fmt.Sprintf(`
import java.util.*;

String input = "%s";

// Parse the JSON array manually
String content = input.substring(1, input.length() - 1);
List<String> chain = new ArrayList<>();
int depth = 0;
StringBuilder current = new StringBuilder();
for (char c : content.toCharArray()) {
    if (c == '"' && depth == 0) { depth = 1; continue; }
    if (c == '"' && depth == 1) {
        chain.add(current.toString());
        current = new StringBuilder();
        depth = 0;
        continue;
    }
    if (depth == 1) current.append(c);
}

chain.add("Java: The end! (JDK " + System.getProperty("java.version") + ", factorial(10) = " + factorial(10) + ")");

System.out.println();
System.out.println("  Final chain (" + chain.size() + " links):");
for (int i = 0; i < chain.size(); i++) {
    System.out.println("    " + (i+1) + ". " + chain.get(i));
}
`, escaped))

	fmt.Println(final)
	fmt.Println()
	fmt.Println("All data flowed: Python -> JS -> Ruby -> Java")
	fmt.Println("on a single OS thread in a single Go process.")
}
