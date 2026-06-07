import { describe, test, expect } from '@jest/globals';
import * as AST from '../src/ast';
import { parseCode } from './helpers';

// Import test helpers for strong AST verification
import {
  verifyGenericType,
  verifyComparison,
  verifyAngleBrackets,
  findInAST,
  findAllInAST
} from './helpers/ast-verifiers';

import {
  findComparisons,
  findChannelOperations,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

describe('Advanced Polyglot Parser Tests', () => {
  test('parses extreme operator mixing and chaining', () => {
    const code = `
# Mixing all operator styles
result := x ?? y || z && w <=> v << 2 >> 1 >>> 3
chan := make(<-chan int)
value := <-chan ?? throw new Error("no value")
ptr := &obj.*.field?.[index]
spread := [...arr1, ...arr2, ...?optionalArr]
pipeline := data |> filter |> map |> reduce
range := 1..10
destructured := {x, y, ...rest} = obj
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify AST structure
    expect(ast.body.length).toBeGreaterThanOrEqual(8);
    
    // Verify first statement is short declaration
    const stmt1 = ast.body[0] as AST.ShortDecl;
    expect(stmt1.kind).toBe('ShortDecl');
    expect(stmt1.pairs![0].name.name).toBe('result');
    
    // Find and verify shift operators in the expression tree
    const allBinary = findAllInAST(ast, n => n && n.kind === 'Binary') as AST.Binary[];
    const shifts = allBinary.filter(n => ['<<', '>>', '>>>'].includes(n.op));
    expect(shifts.length).toBe(3);
    expect(shifts.map(s => s.op).sort()).toEqual(['<<', '>>', '>>>']);
    
    // Verify channel operations
    const channelOps = findChannelOperations(ast);
    expect(channelOps.receives.length).toBeGreaterThan(0);
    
    // Analyze angle bracket usage
    const usage = findAllAngleBracketUsages(ast);
    expect(usage.shifts.leftShift.length).toBe(1);
    expect(usage.shifts.rightShift.length).toBe(1);
    expect(usage.shifts.unsignedRightShift.length).toBe(1);
  });

  test('parses nested mixed-language blocks', () => {
    const code = `
# Python function with Go defer and Ruby blocks
def complexFunc(data):
  defer cleanup()
  
  begin
    if data.valid:
      case data.type in
        "json")
          with open(file) as f:
            loop {
              line := f.readline()
              break if !line
              yield line.strip()
            }
          ;;
        "xml")
          foreach node in data.nodes do
            defer node.close()
            try {
              process(node)
            } catch (e) {
              echo "Error: $e"
            } finally {
              log("processed")
            }
          done
          ;;
      esac
    else:
      raise ValueError("Invalid data")
  rescue => e
    console.error(e)
  ensure
    finalize()
  end
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
    const funcDecl = ast.body[0] as AST.FuncDecl;
    expect(funcDecl.kind).toBe('FuncDecl');
  });

  test('parses mixed async/concurrent patterns', () => {
    const code = `
# Mixing async/await, goroutines, and channels
async fn processStream<T>(input: Stream<T>) -> Result<Vec<T>, Error> {
  results := []
  ch := make(chan T, 100)
  
  // Spawn multiple workers
  for i := 0; i < 10; i++ {
    go async () => {
      while item := <-ch {
        processed := await transform(item)
        results.push(processed)
      }
    }()
  }
  
  // Feed the channel
  async for await (const item of input) {
    select {
      case ch <- item:
        continue
      default:
        await sleep(100)
    }
  }
  
  return Ok(results)
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses complex pattern matching across languages', () => {
    const code = `
# Pattern matching fusion
match value {
  Some(x) if x > 0 => {
    case x in
      1..10)
        echo "small"
        ;;
      11..100)
        echo "medium"
        ;;
      *)
        echo "large"
        ;;
    esac
  },
  None | null | undefined => console.log("empty"),
  [head, ...tail] => process(head, tail),
  {type: "user", name, age} if age >= 18 => authorize(name),
  _ => throw new Error("unmatched")
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses mixed class and trait definitions', () => {
    const code = `
# Classes with mixed syntax
interface AsyncIterator<T> extends Iterable {
  async next(): Promise<{value: T, done: bool}>
}

trait Drawable {
  fn draw(&self)
  def render(self, canvas): pass
}

class Shape implements Drawable with Colorable {
  constructor(public x: number, public y: number) {
    super()
    this.id := generateId()
  }
  
  async def update(self, delta):
    defer self.cleanup()
    with self.lock:
      self.x += delta.x ?? 0
      self.y += delta.y ?? 0
  
  override fn draw(&self) {
    echo "Drawing at ($self.x, $self.y)"
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(3);
  });

  test('parses extreme nesting with mixed syntax', () => {
    const code = `
# Deep nesting across paradigms
function outer() {
  def middle():
    fn inner() -> impl Future {
      async {
        loop {
          for i in 0..10 {
            while true do
              if [ $i -gt 5 ]; then
                case $i in
                  6)
                    begin
                      try:
                        with context:
                          using resource {
                            defer cleanup()
                            yield i
                          }
                      except:
                        pass
                      finally:
                        break
                    end
                    ;;
                  *)
                    continue
                    ;;
                esac
              fi
            done
          }
        }
      }
    }
    return inner
  return middle()
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses mixed string interpolation and templates', () => {
    const code = `
# Every string style
s1 := "basic string"
s2 := 'single quotes'
s3 := \`template \${with} interpolation\`
s4 := f"Python f-string {value}"
s5 := r"raw\\nstring"
s6 := b"byte string"
s7 := """
  multiline
  string
"""
s8 := '''another
multiline'''
s9 := $"C# interpolated {expr}"
s10 := @"verbatim string"
s11 := <<EOF
heredoc content
with $variables
EOF
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(11);
  });

  test('parses mixed type systems and generics', () => {
    const code = `
# Type annotation chaos
type Result<T, E> = Ok<T> | Err<E>
type Maybe<T> = Some(T) | None
interface Functor<F> {
  map<A, B>(f: (a: A) -> B, fa: F<A>): F<B>
}

fn curry<A, B, C>(f: (A, B) -> C) -> A -> B -> C {
  return a => b => f(a, b)
}

async def generic_func[T: Display + Send](
  x: T,
  y: impl Iterator<Item=T>
) -> Result[List[T], str]:
  result: Vec<T> = []
  async for item in y:
    result.push(item)
  return Ok(result)

const typed: {
  readonly [K in keyof T as \`get\${Capitalize<K>}\`]: () => T[K]
} & {
  set<K extends keyof T>(key: K, value: T[K]): void
} = createAccessors()
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(5);
  });

  test('parses mixed comprehensions and generators', () => {
    const code = `
# Comprehension fusion
list1 := [x * 2 for x in range(10) if x % 2 == 0]
set1 := {x for x in items if x.valid}
dict1 := {k: v for k, v in pairs}
gen1 := (x * x for x in numbers)

# Generator functions
function* jsGenerator() {
  yield* otherGen()
  yield 42
}

def pyGenerator():
  yield from another_gen()
  return result

# Async generators
async function* asyncGen() {
  for await (const item of stream) {
    if item.ready:
      yield item.value
  }
}

# Mixed comprehension with pattern matching
result := [
  match x {
    Some(v) if v > 0 => v * 2,
    None => 0,
    _ => -1
  }
  for x in maybeValues
  if x !== undefined
]
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(7);
  });

  test('parses mixed error handling paradigms', () => {
    const code = `
# Error handling fusion
fn risky() -> Result<i32, Error> {
  try {
    value := dangerous_op()?
    
    # Bash-style error check
    if [ $? -ne 0 ]; then
      return Err("Command failed")
    fi
    
    # Go-style error handling
    result, err := another_op()
    if err != nil {
      return Err(err)
    }
    
    # Python-style
    try:
      processed = process(result)
    except ValueError as e:
      raise ChainedError(e)
    else:
      log("success")
    finally:
      cleanup()
    
    # Ruby-style
    begin
      finalize(processed)
    rescue StandardError => e
      retry if retries < 3
    ensure
      close_resources()
    end
    
    # Rust-style
    let final = match processed {
      Ok(v) => v,
      Err(e) => return Err(e.into())
    }
    
    Ok(final)
  } catch (e) {
    Err(e)
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses mixed module and import systems', () => {
    const code = `
# Import/module chaos
import std.stdio
from typing import List, Dict, Optional
import { Component, useState } from 'react'
use std::collections::HashMap
require 'json'
include Math
using System.Linq
package main

# Module definitions
module MyModule {
  export interface Config {
    timeout: number
  }
  
  pub fn setup(config: Config) {
    // implementation
  }
  
  export default class Manager {
    static {
      console.log("Static init")
    }
  }
}

# Namespace
namespace Company.Product {
  export type Settings = Config & {
    advanced: boolean
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(8);
  });

  test('parses mixed decorators and attributes', () => {
    const code = `
# Decorator usage per spec section 13
@deprecated("Use newFunc instead")
@memoize
@log_calls
@inject(Database)
@Override
async def decorated_func(
  @NotNull param1: str,
  @Range(min=0, max=100) param2: int
) -> Result[str, Error]:
  pass

@Component({
  selector: 'app-root',
  template: \`<div>{{title}}</div>\`
})
class AppComponent {
  @Input() title: string
  @Output() clicked = new EventEmitter()
  
  @HostListener('click')
  onClick() {
    this.clicked.emit()
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses polyglot pipe chains with async and closures', () => {
    const code = `
# Real syntax: Elixir pipes, JS async/arrows, Go short decls, Python defs
def extract(source) {
  return fetch(source)
    |> filter(x => x.valid)
    |> map(x => x.value)
    |> reduce((acc, x) => acc + x, 0)
}

async fn transform(data) {
  results := data
    |> filter(item => item.active)
    |> map(item => item.name)
  return results
}

function load(items, config) {
  for item in items {
    try {
      db.insert(item)
    } catch (e) {
      console.error("Failed:", e)
      retry(item, config.max_retries)
    }
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(3);
  });

  test('parses chained operations across paradigms', () => {
    const code = `
# Chaining with spec-compliant operators
result := await fetch(url)
  ?.json()
  ?? defaultValue
  || computeFallback()

# Method chaining with optional chaining
const processed = obj
  .method1()
  ?.method2()
  .method3()
  .filter(x => x?.active ?? false)
  .map(x => x.transform())
  .reduce((acc, x) => acc + x, 0)
  
# Async arrow function
const transformer = async (data) => {
  return data.items
    .filter(x => x !== null)
    .map(x => x * 2)
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(3);
  });

  test('parses extreme concurrency with mixed threads and coroutines', () => {
    const code = `
# Mixing pthreads, coroutines, and async
thread = new Thread(() => {
  co {
    pthread_mutex_lock(&mutex)
    defer pthread_mutex_unlock(&mutex)
    
    async def worker():
      yield coro_suspend()
      await async_task()
      go func() {
        ch <- result
      }()
    
    # PHP coroutine
    $gen = (function() {
      yield from another_gen();
      return $result;
    })();
  }
})

# Bash fork with Ruby threads
fork do
  Thread.new do
    while true; do
      ruby_block { |x|
        synchronized(lock) {
          x.process()
        }
      }
      sleep 1
    done
  end
end
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(2);
  });

  test('parses fused data structures across languages', () => {
    const code = `
# Mixed collections - spec compliant
collection := new ArrayList<HashMap<String, Vector<int>>>()
dict = {key: "value", nested: {inner: 42}}
arr = [1, 2, new LinkedList()]

# Go-style make with maps
slice := make([]map[string]interface{}, 10)
$phpArr = ["extra", true]

# Object literals and arrays
hash = {a: 1, b: 2}
dict = new Dictionary<string, object>()

# Python set syntax
py_set = {1, 2, 3}
java_set = new HashSet<Integer>()
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(6);
  });

  test('parses polyglot file I/O and system calls', () => {
    const code = `
# Mixed I/O
File.open("file.txt", "r") do |f|
  while line = f.gets
    echo $line
  end
done

# PHP with Bash
$handle = fopen("file.txt", "w");
if [ $? -eq 0 ]; then
  fwrite($handle, "data");
  fclose($handle);
fi

# C++ stream with Python
std::ifstream ifs("file.txt");
with open("output.txt", "w") as out:
  for line in ifs:
    out.write(line)

# Go file with Java
f, err := os.Open("file.txt")
if err != nil { return }
defer f.Close()
Scanner scanner = new Scanner(f);
while (scanner.hasNextLine()) {
  System.out.println(scanner.nextLine());
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(4);
  });

  test('parses extreme conditional and loop fusions', () => {
    const code = `
# Nested conditions and loops - spec compliant
if x > 0:
  for z in range(10):
    while w < 5:
      for item in array:
        if condition:
          break
        else:
          continue
          
switch (type) {
  case "int": 
    break;
  default: 
    return;
}

# Bash-style loop
while true do
  echo "Processing"
done
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses mixed networking and HTTP handling', () => {
    const code = `
# Polyglot networking
server = http.createServer((req, res) => {
  $response = new Response();
  go func() {
    conn, err := net.Dial("tcp", "localhost:80")
    if err != nil { return }
    defer conn.Close()
    conn.Write([]byte("GET / HTTP/1.1\\r\\n"))
  }()
  
  # Ruby socket with Python
  socket = Socket.new(:INET, :STREAM)
  with socket.connect(addr):
    socket.send("data")
  
  # C# HttpClient with Java
  HttpClient client = new HttpClient();
  URL url = new URL("http://example.com");
  HttpURLConnection conn = (HttpURLConnection) url.openConnection();
  conn.setRequestMethod("GET");
  using (var response = await client.GetAsync(url)) {
    res.end(response.Content.ReadAsStringAsync());
  }
})
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(1);
  });

  test('parses fused memory management patterns', () => {
    const code = `
# Memory management mix
ptr = malloc(sizeof(int))
defer free(ptr)

# C++ smart pointers with Go GC
unique_ptr<int> up(new int(42));
shared_ptr<int> sp = make_shared<int>(43);
runtime.GC()  # Force GC

# Java try-with-resources with Python context
try (BufferedReader br = new BufferedReader(new FileReader("file"))) {
  with open("file") as f:
    line = br.readLine() or f.readline()
}

# Ruby ensure with C# using
begin
  file = File.open("file")
  using (StreamReader sr = new StreamReader("file")) {
    content = sr.ReadToEnd()
  }
rescue
  # handle
ensure
  file.close if file
end
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(4);
  });

  test('parses polyglot unit testing assertions', () => {
    const code = `
# Mixed testing with spec-compliant syntax
assert x == 1
const result = expect(x).toBe(1)
Assert.AreEqual(1, x)

if x != 1:
  throw new Error("x != 1")

# Simple mocking
mock = Mock()
const stub = sinon.stub(obj, "method")
stub.returns(42)
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(5);
  });

  test('parses extreme regex and pattern handling', () => {
    const code = `
# Regex fusion
regex = /pattern/flags
re = re.compile(r'pattern')
pattern = new Regex(@"pattern")
rx = %r{pattern}i
re2 = regexp.MustCompile("pattern")

# Matching
if regex.test(str) {
  match = str =~ rx
  if match {
    groups = re.match(str).groups()
    std::regex_match(str, sm, re2)
    preg_match('/pattern/', $str, $matches)
  }
}
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(7);
  });

  test('parses mixed build and configuration syntax', () => {
    const code = `
# Build configuration using spec-compliant syntax
const config = {
  "name": "myproject",
  "version": "0.1.0",
  "dependencies": {
    "express": "^4.17.1"
  }
}

# Function calls that look like build commands
project("MyProject")
build("src/*.js")
install("dependencies")

# Simple shell-like commands
echo "Building"
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(4);
  });

  test('parses ultimate operator overloads and custom ops', () => {
    const code = `
# Operator overloads
class Vec {
  def +(other)
    new Vec(x + other.x, y + other.y)
  end
  
  operator+(const Vec& other) const {
    return Vec(x + other.x, y + other.y);
  }
  
  public static Vec operator +(Vec a, Vec b) {
    return new Vec(a.x + b.x, a.y + b.y);
  }
  
  func Add(other Vec) Vec {
    return Vec{x + other.x, y + other.y}
  }
}

# Custom operators
infix operator ** { associativity left precedence 160 }
def **(base, exp)
  base.pow(exp)
end

let a = 2 ** 3 + 4 <=> 5 ? 6 : 7
`;

    const ast = parseCode(code);
    expect(ast.body.length).toBeGreaterThanOrEqual(3);
  });
});