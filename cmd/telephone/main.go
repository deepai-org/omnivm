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

func run(name string, r pkg.Runtime, code string) string {
	result := r.Execute(code)
	if result.Err != nil {
		fmt.Fprintf(os.Stderr, "[%s] error: %v\n", name, result.Err)
		os.Exit(1)
	}
	return strings.TrimRight(result.Output, "\n")
}

func main() {
	py := python.New()
	js := javascript.New()
	rb := ruby.New()
	jv := jvm.New()

	fmt.Fprintln(os.Stderr, "Initializing 4 runtimes on the Golden Thread...")
	must(py, py.Initialize())
	must(js, js.Initialize())
	must(rb, rb.Initialize())
	must(jv, jv.Initialize())
	fmt.Fprintln(os.Stderr, "All runtimes ready.\n")

	fmt.Println("=== OmniVM Telephone Game ===")
	fmt.Println("One process. One thread. Four runtimes.")
	fmt.Println()

	// Step 1: Python creates the initial data
	fmt.Println("[1/4] Python starts the chain...")
	data := run("python", py, `
import json
chain = ["Python: The quick brown fox (sum 1..10 = " + str(sum(range(1,11))) + ")"]
print(json.dumps(chain))
`)
	fmt.Printf("      data = %s\n\n", data)

	// Step 2: JavaScript receives, appends, passes on
	fmt.Println("[2/4] JavaScript receives and appends...")
	data = run("javascript", js, fmt.Sprintf(`
var chain = %s;
chain.push("JavaScript: jumps over " + (6 * 7) + " lazy dogs");
console.log(JSON.stringify(chain));
`, data))
	fmt.Printf("      data = %s\n\n", data)

	// Step 3: Ruby receives, appends, passes on
	fmt.Println("[3/4] Ruby receives and appends...")
	escapedForRuby := strings.ReplaceAll(data, "'", "\\'")
	data = run("ruby", rb, fmt.Sprintf(`
require 'json'
chain = JSON.parse('%s')
chain << "Ruby: while eating #{Math::PI.round(4)} pies"
puts JSON.generate(chain)
`, escapedForRuby))
	fmt.Printf("      data = %s\n\n", data)

	// Step 4: Java receives, appends, prints final report
	fmt.Println("[4/4] Java receives and delivers final report...")
	escapedForJava := strings.ReplaceAll(data, `"`, `\"`)
	final := run("java", jv, fmt.Sprintf(`
String input = "%s";
// parse JSON array
String inner = input.substring(1, input.length() - 1);
java.util.List<String> chain = new java.util.ArrayList<>();
boolean inStr = false;
StringBuilder sb = new StringBuilder();
for (int i = 0; i < inner.length(); i++) {
    char c = inner.charAt(i);
    if (c == '"' && (i == 0 || inner.charAt(i-1) != '\\')) {
        if (inStr) { chain.add(sb.toString()); sb.setLength(0); }
        inStr = !inStr;
    } else if (inStr) {
        sb.append(c);
    }
}
// compute factorial(10) in Java
long fact = 1;
for (int i = 1; i <= 10; i++) fact *= i;
chain.add("Java: The end! (factorial(10) = " + fact + ")");

System.out.println();
System.out.println("      Final chain (" + chain.size() + " links):");
for (int i = 0; i < chain.size(); i++) {
    System.out.println("        " + (i+1) + ". " + chain.get(i));
}
System.out.println();
System.out.println("      All data passed through Python -> JS -> Ruby -> Java");
System.out.println("      on a single OS thread, in a single Go process.");
`, escapedForJava))

	fmt.Println(final)

	os.Stdout.Sync()
}
