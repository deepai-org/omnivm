# Claude Development Guidelines

## Project Overview
PolyScript is a universal parser that handles multiple programming language syntaxes in a single file.

## Current Status
- **630/630 tests passing (100% pass rate)**
- Complete type parsing and multi-paradigm support
- Rust-style syntax fully supported (::, async move, .await, ? try operator)
- Match statements with multiple arms working correctly
- Deep nested generic types (15+ levels tested) parsing successfully
- JSX with generic type arguments fully implemented per spec
- Type assertions (`<Type>expr`) correctly disambiguated from JSX
- List comprehensions with multiple target variables
- C# property detection with type + name + accessor pattern
- All major AST data recovery complete

## Quick Commands
```bash
npm test          # Run all tests
npm run build     # Build TypeScript
```

## Key Parser Issues & Solutions

### Virtual Semicolons
- Skip them in pattern matching: `while (this.peek().virtualSemi) this.advance()`
- They're auto-inserted by lexer, handle carefully

### Infinite Loops
- Always ensure loops either consume a token or break
- Add safeguards: `if (this.current === beforePos) this.advance()`

### Context-Dependent Tokens
- `<` can be comparison, generic start, JSX element, or type assertion
  - Use `couldBeTypeAssertion()` for disambiguation
  - Check for closing tags to confirm JSX vs type assertion
  - Look for JSX attributes vs expression continuations
- `:` can be type annotation, case separator, or Python block start
- `.` triggers MemberAccess mode where keywords become identifiers

### Parser Structure
- Main flow: `parse()` → `parseTopLevel()` → `parseStatement()` or `parseDeclaration()`
- Function bodies use `parseBlock()`, not `parseTopLevel()`
- `braceDepth` tracks nesting for proper `}` handling

## Lexer Mode Stack
The lexer has 5 modes that change tokenization behavior:

1. **Normal** - Default mode
2. **MemberAccess** - After `.`, keywords → identifiers
3. **BashCondition** - Inside `[ ]` for bash tests
4. **Decorator** - After `@`, keywords → identifiers
5. **StringTemplate** - For special string literals

## Debug Workflow
1. **Isolate** - Extract failing code from test
2. **Simplify** - Reduce to minimal case
3. **Inspect** - Check tokens with: `tokens.forEach(t => console.log(t))`
4. **Trace** - Add logging to suspicious parser methods
5. **Fix** - Make minimal change
6. **Verify** - Test variations before running full suite
