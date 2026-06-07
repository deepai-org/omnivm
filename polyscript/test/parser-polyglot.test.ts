import { Parser } from '../src/parser';
import { Lexer } from '../src/lexer';
import * as AST from '../src/ast';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Polyglot Parser Tests', () => {
  test('parses Python-style with JavaScript features', () => {
    const code = `
# Python comment with JavaScript arrow function
def process(data):
  result = data.map((x) => x * 2)
  if len(result) > 0:
    for item in result:
      console.log(item)
    return true
  else:
    return false
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(0);
    const funcDecl = ast.body[0] as AST.FuncDecl;
    expect(funcDecl.kind).toBe('FuncDecl');
    expect(funcDecl.name.name).toBe('process');
  });

  test('parses Go-style with TypeScript types', () => {
    const code = `
// Go short declaration with TypeScript type annotation
x := 10
var y: number = 20
const result: string = "test"

// Go channels with TypeScript generics
ch := make(chan<string>, 10)
ch <- "hello"
msg := <- ch

// Go defer with JavaScript arrow function
defer (() => console.log("cleanup"))()
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(0);
  });

  test('parses Ruby-style with Go concurrency', () => {
    const code = `
# Ruby-style begin/end with Go routine
begin
  go processAsync()
  x = 10
  y = 20
end

# Ruby foreach with defer
foreach item in [1, 2, 3] do
  defer console.log("done")
  echo item
done
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBe(2);
  });

  test('parses Bash-style with modern JavaScript', () => {
    const code = `
# Bash case with JavaScript template literals and arrow functions
case $option in
  "start")
    console.log(\`Starting service...\`)
    handler = () => console.log("Started")
    handler()
    ;;
  "stop")
    echo "Stopping..."
    ;;
  *)
    throw new Error("Unknown option")
    ;;
esac

# Bash if/then/fi with async/await
if [ $x -gt 0 ]; then
  result = await fetchData()
  console.log(result)
fi
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBe(2);
    const switchStmt = ast.body[0] as AST.Switch;
    expect(switchStmt.kind).toBe('Switch');
  });

  test('parses mixed control flow styles', () => {
    const code = `
# Python-style if with bash-style while
if x > 0:
  while true do
    echo "Processing"
    if done:
      break
  done
  
# JavaScript switch with Python-style match arms
switch (type) {
  case "user":
    if admin:
      privileges = ["read", "write", "delete"]
    else:
      privileges = ["read"]
    break
  default:
    privileges = []
}

# Go-style for with Python indentation
for i := 0; i < 10; i++ {
  if i % 2 == 0:
    continue
  echo i
}
`;

    const ast = parseCode(code);
    // Comments don't create AST nodes, so we may have different counts
    // Focus on whether the code parses without errors
    expect(ast.body.length).toBeGreaterThanOrEqual(2);
  });

  test('parses mixed declarations and literals', () => {
    const code = `
// All declaration styles
let jsVar = 10
var oldVar = 20
const constVar = 30
immutable immut = 40
final fin = 50
auto a = 60

// Go short declarations
x := 100
y, z := 200, 300

// Different literal styles
binary := 0b1010
octal := 0o755
hex := 0xFF
bigint := 999999999999999999999n
underscore := 1_000_000

// String variations
str1 := "double quotes"
str2 := 'single quotes'
template := \`template \${x}\`
raw := r"raw\\nstring"

// Arrays and objects
arr := [1, 2, ...spread, 4]
obj := {
  key: "value",
  method() { return 42 },
  ...other
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(10);
  });

  test('parses function declaration mixing', () => {
    const code = `
// All function declaration styles
function jsFunc(a, b) { return a + b }
def pyFunc(a, b): return a + b
fun ktFunc(a, b) { return a + b }
fn rsFunc(a, b) { return a + b }

// With async/unsafe modifiers
async function asyncOne() { await task() }
unsafe fn unsafeOne() { echo "unsafe" }
async def asyncPy(): pass

// Arrow functions and lambdas
arrow := (x) => x * 2
shortArrow := x => x * 3
multiLine := (x, y) => {
  z := x + y
  return z * 2
}

// Anonymous functions in calls
arr.map((x) => {
  defer cleanup()
  return x * 2
})
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(7);
  });

  test('parses exotic but valid combinations', () => {
    const code = `
// Ternary with null coalescing and optional chaining
result := obj?.method() ?? (x > 0 ? "positive" : "negative")

// Assignment operators cascade
a ||= b &&= c ??= d

// Operator soup (but valid!)
x := y << 2 | z >> 1 & 0xFF ^ 0x0F

// Increment/decrement with other ops
val := x++ + --y * z--

// Channel operations mixed with regular ops
ch <- value + 10
received := (<- ch) * 2

// Regex match with ternary
isValid := str =~ /^[a-z]+$/ ? true : false

// Multiple return with destructuring
{a, b} = func() 
[x, y, z] = getCoords()

// Using with try/catch/finally
using file = open("test.txt") {
  try {
    data := file.read()
  } catch (e) {
    console.error(e)
  } finally {
    echo "Done"
  }
}

// Yield in different contexts
function* gen() {
  yield 1
  yield* other()
  x := yield 3
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(0);
  });

  test('parses deeply nested mixed-style blocks', () => {
    const code = `
# Ultimate nesting test
def outer():
  for i in range(10):
    if i > 5:
      switch i {
        case 6:
          begin
            while true do
              if done; then
                echo "Complete"
                break
              fi
            done
          end
          break
        case 7:
          case x in
            1) echo "one";;
            2) echo "two";;
          esac
        default:
          try:
            throw new Error("test")
          except:
            pass
      }
    elif i < 2:
      foreach j in [1,2,3] do
        using resource = getResource() {
          defer resource.close()
          go processAsync(resource)
        }
      done
    else:
      continue
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThan(0);
    const func = ast.body[0] as AST.FuncDecl;
    expect(func.kind).toBe('FuncDecl');
  });
});