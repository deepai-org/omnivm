import { Parser } from '../src/parser';
import { Lexer } from '../src/lexer';
import * as AST from '../src/ast';

// Import test helpers
import {
  verifyFunctionDecl,
  verifyChannelSend,
  verifyChannelReceive,
  findInAST,
  findAllInAST,
  findFirst,
  findByKind
} from './helpers/ast-verifiers';

import {
  findChannelOperations,
  findAllAngleBracketUsages,
  analyzeAngleBrackets
} from './helpers/pattern-matchers';

function parseCode(code: string): AST.Program {
  const lexer = new Lexer(code);
  const tokens = lexer.tokenize();
  const parser = new Parser(tokens);
  return parser.parse();
}

describe('Polyglot Parser Tests (UPDATED)', () => {
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
    
    // ✅ STRONG: Verify Go-style short declarations
    expect(ast.body.length).toBeGreaterThan(0);
    
    // Find short declarations (x := 10)
    const shortDecls = findByKind<AST.ShortDecl>(ast, 'ShortDecl');
    expect(shortDecls.length).toBeGreaterThanOrEqual(2); // x and msg at least
    
    // Find typed declarations
    const varDecl = findFirst<AST.VarDecl>(ast, n => 
      n.kind === 'VarDecl' && n.names[0]?.name === 'y'
    );
    expect(varDecl).toBeDefined();
    if (varDecl) {
      expect(varDecl.names[0].name).toBe('y');
      expect(varDecl.type).toBeDefined();
    }
    
    // Find channel operations
    const channelOps = findAllAngleBracketUsages(ast);
    expect(channelOps.channels.sends.length).toBeGreaterThan(0); // ch <- "hello"
    expect(channelOps.channels.receives.length).toBeGreaterThan(0); // <- ch
    
    // Find defer statement
    const deferStmt = findFirst(ast, n => n.kind === 'Defer');
    expect(deferStmt).toBeDefined();
    
    // Verify angle bracket analysis
    const stats = analyzeAngleBrackets(ast);
    expect(stats.channelCount).toBeGreaterThanOrEqual(2);
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
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify different function declarations
    expect(ast.body.length).toBeGreaterThan(7);
    
    // Find all function declarations
    const funcDecls = findByKind<AST.FuncDecl>(ast, 'FuncDecl');
    expect(funcDecls.length).toBeGreaterThanOrEqual(7);
    
    // Verify specific function names
    const funcNames = funcDecls.map(f => f.name.name);
    expect(funcNames).toContain('jsFunc');
    expect(funcNames).toContain('pyFunc');
    expect(funcNames).toContain('ktFunc');
    expect(funcNames).toContain('rsFunc');
    expect(funcNames).toContain('asyncOne');
    expect(funcNames).toContain('unsafeOne');
    expect(funcNames).toContain('asyncPy');
    
    // Verify async functions
    const asyncFunc = funcDecls.find(f => f.name.name === 'asyncOne');
    expect(asyncFunc?.async).toBe(true);
    
    // Verify unsafe function
    const unsafeFunc = funcDecls.find(f => f.name.name === 'unsafeOne');
    expect(unsafeFunc?.unsafe).toBe(true);
    
    // Find arrow functions (stored as short declarations)
    const shortDecls = findByKind<AST.ShortDecl>(ast, 'ShortDecl');
    const arrowDecls = shortDecls.filter(d => 
      d.pairs && d.pairs[0] && ['arrow', 'shortArrow', 'multiLine'].includes(d.pairs[0].name.name)
    );
    expect(arrowDecls.length).toBeGreaterThanOrEqual(3);
    
    // Verify arrow function values are lambdas
    arrowDecls.forEach(decl => {
      expect(decl.pairs![0].expr.kind).toBe('Lambda');
    });
  });

  test('parses mixed error handling patterns', () => {
    const code = `
// Try-catch-finally (JavaScript/Java)
try {
  risky()
} catch (e) {
  console.error(e)
} finally {
  cleanup()
}

// Try-except (Python)
try {
  danger()
} except (ValueError) {
  print("value error")
}

// Try-rescue (Ruby)
try {
  unsafe()
} rescue (e) {
  puts e
}

// Error propagation (Rust-style)
result := doWork()?

// Panic/recover (Go-style)
defer recover()
panic("error")
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify different error handling styles
    expect(ast.body.length).toBeGreaterThan(0);
    
    // Find try statements
    const tryStmts = findByKind<AST.Try>(ast, 'Try');
    expect(tryStmts.length).toBeGreaterThanOrEqual(3);
    
    // Verify different catch styles
    tryStmts.forEach(tryStmt => {
      expect(tryStmt.catches.length).toBeGreaterThanOrEqual(1);
    });
    
    // Check for finally block
    const withFinally = tryStmts.find(t => t.finallyBody !== undefined);
    expect(withFinally).toBeDefined();
    
    // Find defer statements
    const deferStmts = findByKind(ast, 'Defer');
    expect(deferStmts.length).toBeGreaterThanOrEqual(1);
    
    // Find panic (might be parsed as call)
    const panicCall = findFirst(ast, n => 
      n.kind === 'Call' && n.func?.name === 'panic'
    );
    expect(panicCall).toBeDefined();
  });

  test('parses pattern matching across languages', () => {
    const code = `
// Rust-style match
match value {
  Some(x) => println(x),
  None => println("empty"),
  _ => println("other")
}

// Switch with pattern matching
switch (type) {
  case "user":
    handleUser()
    break
  case "admin":
    handleAdmin()
    break
  default:
    handleGuest()
}

// Bash case
case $option in
  start)
    echo "Starting"
    ;;
  stop)
    echo "Stopping"
    ;;
  *)
    echo "Unknown"
    ;;
esac

// When expression (Kotlin-style)
result := when (x) {
  1 => "one"
  2 => "two"
  else => "many"
}
`;

    const ast = parseCode(code);
    
    // ✅ STRONG: Verify different pattern matching styles
    expect(ast.body.length).toBeGreaterThan(0);
    
    // Find switch/match statements
    const switches = findByKind<AST.Switch>(ast, 'Switch');
    expect(switches.length).toBeGreaterThanOrEqual(2);
    
    // Find match statements specifically
    const matches = findByKind<AST.Match>(ast, 'Match');
    expect(matches.length).toBeGreaterThanOrEqual(1);
    
    if (matches.length > 0) {
      // Verify match arms
      const match = matches[0];
      expect(match.arms.length).toBeGreaterThanOrEqual(3);
      
      // Check for wildcard pattern
      const wildcardArm = match.arms.find(arm => 
        arm.patterns[0]?.kind === 'Identifier' && (arm.patterns[0] as any).name === '_'
      );
      expect(wildcardArm).toBeDefined();
    }
    
    // Verify case statements have proper structure
    switches.forEach(sw => {
      expect(sw.cases.length).toBeGreaterThanOrEqual(2);
      
      // Check for default case
      if (sw.defaultCase) {
        expect(sw.defaultCase.statements.length).toBeGreaterThan(0);
      }
    });
  });

  // Keep existing strong tests
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
    
    // Verify function body structure
    const body = funcDecl.body as AST.Block;
    expect(body.statements.length).toBeGreaterThan(0);
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
    expect(switchStmt.cases.length).toBeGreaterThanOrEqual(3);
    
    // Verify if statement
    const ifStmt = ast.body[1] as AST.If;
    expect(ifStmt.kind).toBe('If');
  });
});