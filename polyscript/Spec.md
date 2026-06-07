# PolyScript 1.0 — Complete Language Specification

*(max-compatibility edition: every valid program in JavaScript, Python, TypeScript, Java, C#, C++, Go, PHP, Bash, or Ruby is valid PolyScript without change)*

---

## 0. Notation

* EBNF metasymbols: `X?` optional, `X*` zero-or-more, `X+` one-or-more, `A | B` alternation, `( … )` grouping.
* “Token” = lexeme produced by the lexical phase (§2).
* “Core grammar” = canonical AST in §4; all surface forms lower to it.
* A *compile-time error* aborts translation. A *run-time error* raises an `Error`.
* Files may start with a UTF-8 BOM; it is ignored.

---

## 1. Source file

1. Text is UTF-8.
2. A shebang is recognized only if the very first two bytes are `#!` and runs up to the first `\n`. The compiler ignores it but preserves it in emitted scripts.
3. File extension has no semantics.

---

## 2. Lexical specification

### 2.1 Character classes

```
Letter      = UnicodeLetter | "_" ;
Digit       = "0"…"9" ;
Whitespace  = 0x20 | TAB | VT | FF ;         // ASCII space, horizontal tab, vertical tab, form feed
Linebreak   = CR LF | LF | CR ;
```

Unless stated, other Unicode spaces are ordinary characters.

### 2.2 Tokenisation order

Lexer scans left-to-right with longest-match. The table is applied in order.

| #  | Pattern                                 | Emits                                 |                           |
| -- | --------------------------------------- | ------------------------------------- | ------------------------- |
| 1  | Shebang at file start (`#!…\n`)         | *Discard*                             |                           |
| 2  | `//` to Linebreak                       | `Comment`                             |                           |
| 3  | `/* … */`                               | `Comment`                             |                           |
| 4  | `<!-- … -->`                            | `Comment`                             |                           |
| 5  | `#` to Linebreak (not part of shebang)  | `Comment`                             |                           |
| 6  | `--` to Linebreak                       | `Comment`                             |                           |
| 7  | **Backtick probe** (§2.3.1)             | `TemplateLiteral` (subject to §2.3.1) |                           |
| 8  | String and template literals (§2.4)     | `StringLiteral` / `TemplateLiteral`   |                           |
| 9  | Slash/Regex decision (§2.5)             | `/` or `RegexLiteral`                 |                           |
| 10 | Numeric literal (§2.6)                  | `NumericLiteral`                      |                           |
| 11 | `$` Letter (Letter                      | Digit)\*                              | `SigilIdentifier`         |
| 12 | Letter (Letter                          | Digit)\*                              | `Identifier` or `Keyword` |
| 13 | Operators & punctuators (longest first) | `Operator`                            |                           |
| 14 | Whitespace or Linebreak                 | *Discard* or `VirtualSemi` (§2.7)     |                           |

Comments and whitespace never reach the parser.

**Operators and punctuators (non-exhaustive, longest first):**
`>>>==`, `>>>=`, `>>>`, `>>=`, `<<=`, `===`, `!==`, `??=`, `**=`, `...`, `=>`, `==`, `!=`, `<=`, `>=`, `<<`, `>>`, `&&`, `||`, `??`, `**`, `+=`, `-=`, `*=`, `/=`, `%=` and all single-char punctuators `()[]{},.;:?+-*/%&|^~!<>=#@`.

### 2.3 Identifiers

```
Identifier         = Letter (Letter | Digit)* ;
SigilIdentifier    = "$" Identifier ;               // presentational alias
BacktickIdentifier = see §2.3.1 ;
```

Identifiers are case-sensitive and Unicode-normalized (NFC).
A keyword (§10) may be used as an identifier only via **backticks** (§2.3.1), except in object property positions where keywords are allowed directly (e.g., `{ async: true, for: 42 }`).
`$foo` and `foo` denote the same symbol everywhere.

#### 2.3.1 Backtick identifiers (contextual)

Backticks lex as JavaScript template literals. The parser **reinterprets** a `TemplateLiteral` as an `Identifier` **only in identifier positions** (declaration names, labels, non-computed member keys, `import … as …`) **if**:

* the template has no `${…}` segments, and
* its content matches `^[A-Za-z_][A-Za-z0-9_]*$`.

The backticks are not part of the name. In expression positions backticks are always strings.

### 2.4 String and template literals

Allowed delimiters: `'…'`, `"…"`, `'''…'''`, `"""…"""`, `` `…` ``, Ruby percent literals `%x{…}` family (`%q`, `%r`, `%w`, `%i`, `%s`), Bash `$'…'`, and C# verbatim forms `$"…", @$"…", @$'…'`.

**Prefixes:** `r`, `b`, `f`, `c` may appear in any order; duplicates ignored. Effects are cumulative where compatible.

* `r` (raw): disables escapes for `'`/`"`/triple-quoted. No effect on `` `…` `` which follows JS raw-segment rules.
* `b` (bytes): result type is `bytes` using UTF-8 after any interpolation and escape processing (unless `r` suppressed escapes). Not allowed with `` `…` ``. Compile-time error if the final byte sequence is not valid UTF-8.
* `f` (format): enables `${…}` interpolation inside `'`/`"`/triple-quoted forms; evaluated at run time unless `c` also present.
* `c` (const): every `${…}` expression must be a compile-time constant; otherwise a compile-time error. With `` `…` `` this imposes the same requirement on its `${…}` segments.
* Template literals `` `…` `` follow JavaScript lexical rules including line breaks and segmentation.

### 2.5 Regex vs division

Let **Prev** be the last non-whitespace, non-comment token. `/` starts a `RegexLiteral` **iff** `Prev` **cannot end an expression**.

Tokens that **can end** an expression: `Identifier`, `SigilIdentifier`, `NumericLiteral`, `StringLiteral`, `TemplateLiteral`, `RegexLiteral`, `]`, `)`, `}`, `++`, `--`.

All other preceding tokens cause `/` to begin a regex (for example after `(`, `{`, `[`, `,`, `;`, assignment or comparison operators, `!`, unary operators, `?`, `:`, `??`, `=>`, and after `return`, `yield`, `throw`, `case`, `delete`, `void`, `typeof`, `in`, `instanceof`, `new`, `do`, `else`).
Regex flags follow JavaScript (`/…/gimsuy`). Line breaks inside `/…/` are forbidden unless escaped.

**Soft overrides:** the contextual identifiers `regex` and `div` may be written immediately before `/` to force interpretation. They are ordinary identifiers elsewhere.

### 2.6 Numeric literals

The sign is **not** part of the literal; `+` and `-` are unary operators.

```
IntegerLiteral ::= ("0x" HexDigits | "0o" OctDigits | "0b" BinDigits | DecDigits)
                   ("_" Digits)* [ "n" ] [ TypeSuffix ] ;

FloatLiteral   ::= DecDigits "." DecDigits? Exponent?
                 | "." DecDigits Exponent? ;

Exponent       ::= [eE] [+-]? DecDigits ;

TypeSuffix     ::= ("u"|"U"|"l"|"L"|"f"|"F")
                 | ("i8"|"i16"|"i32"|"i64"|"u8"|"u16"|"u32"|"u64"|"f32"|"f64") ;
```

Underscores may not be first or last in a run of digits and may not be adjacent to `.` or base prefixes.
A trailing `n` denotes a `bigint` literal and may not appear with `TypeSuffix`.
After lexing, numeric literals are typed per §6.3.

### 2.7 MASI — Max-Accept Semicolon Insertion

* A physical `;` always terminates the current statement.
* At each `Linebreak` the lexer emits `VirtualSemi` **unless** any hold:

  1. the line’s last non-space character is one of `.,:;+-*/%&|^<>=!~?([{`
  2. the current `()[]{}` depth > 0
  3. the next non-blank line is **strictly more indented** than the previous non-blank line.

A `VirtualSemi` is semantically identical to `;`.

**Indentation measurement:** tabs expand to columns at multiples of 8. Indentation is the column count before the first non-space character. Mixing tabs and spaces is allowed; measurement is by columns.

---

## 3. Block delimiters

Three independent stacks:

| Stack   | Opens on                                                                             | Closes on                   |
| ------- | ------------------------------------------------------------------------------------ | --------------------------- |
| Brace   | `{`                                                                                  | `}`                         |
| Indent  | a **header** ending with `:` followed by a deeper-indented next non-blank line       | out-dent to previous level  |
| Keyword | `do`, `case`, `begin`, `if`(Ruby/Bash), `for`(Ruby), `while`(Ruby), `function`(Bash) | `done`, `esac`, `end`, `fi` |

**Note on `do` disambiguation:** The keyword `do` followed by `{` or an indented block and then `while` forms a JavaScript-style do-while loop (§8.1). Otherwise, `do` expects `done` as its closer (Bash-style).

**Header (Indent stack):** `if`/`elif`/`else`/`for`/`while`/`def`/`fun`/`function`/`class`/`try`/`except`/`finally`/`match`/`switch`.
A block ends when its own closer appears first. Crossing stacks is a compile-time error.

---

## 4. Core grammar (canonical AST)

```
Program        ::= { Declaration | Statement } ;
Declaration    ::= Import | VarDecl | ConstDecl | ShortDecl | FuncDecl | TypeDecl ;
Statement      ::= ExprStmt | If | Loop | Switch | Try | Using | Defer | Break | Continue | Return | Echo ;
Expr           ::= Literal | Ident | Lambda | Call | Index | Member | Unary | Binary | Assign | JSXElement ;
Type           ::= Simple | Nullable | Generic | Union | FuncType | IndexedAccess ;
```

All surface forms from donor languages are deterministically rewritten to this core.

---

## 5. Generics and `<` disambiguation

1. When `<` follows an `Identifier` **with no intervening whitespace or linebreak**, the parser tentatively parses a type-argument list.
2. The tentative parse must be followed by one of: `(` `[` `{` `>` `>>` `>>>` `:` `extends` `implements` `where`, or an appropriate type use site.
3. If the tentative parse fails, `<` is an operator.
4. Directive comments `// @generics` or `// @nogenerics` force the choice for the **next statement only**. Directives are removed after parsing.

---

## 6. Type system

### 6.1 Universe

```
any       // top
never     // bottom
bool, bytes, string, char, bigint

i8  i16  i32  i64          // signed
u8  u16  u32  u64          // unsigned
f32 f64

class  interface  struct  trait  enum

NullableType := Type "?"
GenericType  := Ident "<" Type { "," Type } ">" | Ident "[" Type { "," Type } "]"
UnionType    := Type "|" Type { "|" Type }
IndexedAccessType := Type "[" StringLiteral "]"  // TypeScript-style indexed access e.g., Type["property"]

chan<T>     // channel type used by §12.4
```

`null` is the sole null value. `undefined` is an alias of `null`.

### 6.2 Gradual rules

If an expression lacks explicit annotation and cannot be inferred, its static type is `any`.
Type errors inside `any` regions are detected only at run time.

### 6.3 Literal narrowing

An unsuffixed **integer** literal is assigned the narrowest of `i32`, `u32`, `i64`, `u64` that exactly contains its value; otherwise `bigint`. For `-42`, narrowing uses the magnitude of `42` and then applies unary `-`.
`42L` forces 64-bit signed, `1_000_000u` → `u32`.
Float literals default to `f64` unless suffixed.

---

## 7. Declarations

### 7.1 Variables

All donor forms lower to either mutable (`var`) or immutable (`const`) symbols.

```
VarDecl    ::= ("let" | "var" | "auto") IdentifierList [ ":" Type ] [ "=" ExprList ] ;
ConstDecl  ::= ("const" | "final" | "immutable") IdentifierList [ ":" Type ] "=" ExprList ;
ShortDecl  ::= Identifier ":=" Expr { "," Identifier ":=" Expr } ;
```

`SigilIdentifier` is a presentation of `Identifier`; it is never a declaration keyword.

**Reassignment operator `:=:`** (expression position) rebinds an existing variable; compile-time error if the name is not already defined in the current scope.

### 7.2 Functions

```
FuncDecl ::=
  [ "async" ] [ "unsafe" ]
  ( ("def" | "fun" | "fn" | "function") Identifier ParamClause [ ReturnClause ]
  | ReturnTypeBefore Identifier ParamClause ) Body ;

ParamClause     ::= "(" [ Param { "," Param } ] ")" ;
Param           ::= Identifier [ ":" Type ] [ "=" Expr ] ;

ReturnClause    ::= "->" Type | ":" Type ;   // first wins
ReturnTypeBefore::= Type ;                   // e.g., "int add(a:int, b:int)"

Body            ::= Block | "=>" Expr ;      // JS/Ruby “stabby” allowed
```

If both a leading return type and a trailing `ReturnClause` are present, it is a compile-time error.

### 7.3 Types

Surface keywords `class`, `struct`, `interface`, `trait`, `enum` are accepted with C/Java/Go/PHP/Rust-style clauses; all rewrite to the core nominal-type node.

---

## 8. Statements

### 8.1 Control flow

```
If       ::= "if" Expr Block { ElseIf } [ Else ] ;
ElseIf   ::= ("elif" | "elseif") Expr Block ;
Else     ::= "else" Block ;

Loop     ::= For | While | DoWhile | Until | Foreach | Infinite ;
DoWhile  ::= "do" Block "while" "(" Expr ")" [ ";" ] ;
Switch   ::= ("switch" | "match" | "case") Expr Block ;

Break    ::= "break" [ Identifier ] ;
Continue ::= "continue" [ Identifier ] ;
Return   ::= "return" [ ExprList ] ;
Echo     ::= ("echo" | "print") ExprList ;
```

`Switch` arms do **not** fall through. Fall-through is enabled only if the next arm is immediately preceded by the exact token `fallthrough;` or a single-line comment exactly equal to `// fallthrough`.

### 8.2 Resource management

`using`, `with`, `defer`, and C++ RAII destructors compile to `try … finally`.

---

## 9. Expressions

Evaluation order is left-to-right except short-circuiting.

```
a && b, a || b, a ?? b   → short-circuit
x ? y : z                → ternary
<=>                      → Ruby spaceship
```

Fixed precedence (highest to lowest):

1. call `()`, index `[]`, member `.`, optional chain `?.`
2. `new`(with args), postfix `++ --`
3. unary `! ~ + - typeof void delete await`, prefix `++ --`
4. `**` (right-associative)
5. `* / %`
6. `+ -`
7. `<< >> >>>`
8. `< <= > >= in instanceof`
9. `== != === !== =~`
10. `&`
11. `^`
12. `|`
13. `&&`
14. `||`
15. `??`
16. `?:`
17. assignments `=, +=, -=, *=, /=, %=, **=, <<=, >>=, >>>=, &=, ^=, |=, ??=, :=, :=:` (right-associative)
18. `,` sequence

#### 9.1 Assignment and equality spellings

| Spelling | Meaning                                          |
| -------- | ------------------------------------------------ |
| `=`      | assignment                                       |
| `:=`     | short declaration (new variable)                 |
| `:=:`    | reassignment of existing variable (shadow check) |
| `==`     | loose equality (string-numeric coercion)         |
| `===`    | strict identity                                  |
| `=~`     | regex match (`lhs =~ /re/`)                      |

`+` with either operand `string` concatenates.

---

## 10. JSX/TSX Syntax

### 10.1 JSX Elements

JSX elements are first-class expressions in PolyScript, enabling React, Preact, Solid, and similar frameworks.

```
JSXElement     ::= JSXSelfClosing | JSXContainer ;
JSXSelfClosing ::= "<" JSXElementName JSXAttributes? "/>" ;
JSXContainer   ::= JSXOpeningElement JSXChildren? JSXClosingElement ;

JSXOpeningElement ::= "<" JSXElementName JSXAttributes? ">" ;
JSXClosingElement ::= "</" JSXElementName ">" ;

JSXElementName ::= Identifier                           // Component
                 | Identifier ("." Identifier)+         // Namespaced
                 | LowerCaseIdentifier                  // HTML element
                 ;

JSXChildren    ::= (JSXText | JSXElement | JSXExpression | JSXFragment)+ ;
JSXText        ::= any text except "{", "<", ">", "&" (with entity escapes) ;
JSXExpression  ::= "{" Expr "}" | "{" "..." Expr "}" ;  // Spread children
JSXFragment    ::= "<>" JSXChildren? "</>" ;
```

### 10.2 JSX Attributes

```
JSXAttributes     ::= (JSXAttribute | JSXSpreadAttribute)* ;
JSXAttribute      ::= JSXAttributeName [ "=" JSXAttributeValue ] ;
JSXSpreadAttribute::= "{" "..." Expr "}" ;

JSXAttributeName  ::= Identifier | Identifier (":" | "-") Identifier ;
JSXAttributeValue ::= StringLiteral 
                    | "{" Expr "}"
                    | JSXElement
                    | JSXFragment ;
```

### 10.3 JSX Type Annotations (TSX)

In `.tsx` contexts or when types are enabled:

```
JSXGenericElement ::= "<" JSXElementName TypeArguments JSXAttributes? ">" ;
TypeAssertion     ::= Expr "as" Type ;                 // In JSX expressions
JSXTypeAttribute  ::= Identifier ":" Type "=" JSXAttributeValue ;
```

### 10.4 JSX Lexical Rules

1. **Context switching**: After `<` followed by an identifier or `>` (fragment), the lexer enters JSX mode.
2. **JSX mode exits** on: matching `>` or `/>` at element depth 0.
3. **Nested elements**: Track depth for proper closing.
4. **Expression holes**: `{` in JSX mode starts an expression context until matching `}`.
5. **Text content**: Between tags, text is preserved with whitespace normalization:
   - Leading/trailing whitespace on lines with only whitespace is removed
   - Newlines become single spaces unless preserved by `{' '}`
   - HTML entities (`&lt;`, `&gt;`, `&amp;`, `&quot;`, `&#...;`) are recognized

### 10.5 JSX Comments

```
JSXComment ::= "{/*" (any except "*/")* "*/}" ;
```

### 10.6 JSX Disambiguation

JSX elements are recognized by **pattern-based disambiguation** when `<` appears in **expression contexts**.

#### 10.6.1 JSX Expression Contexts

JSX is valid in these contexts:
- Expression statements: `<Button />`
- Assignments: `const x = <Component />`
- Return statements: `return <div>content</div>`
- Ternary operators: `condition ? <Success /> : <Error />`
- Array/object literals: `[<Item />]`, `{ header: <Header /> }`
- Function arguments: `render(<Component />)`
- JSX children: `<div>{<Child />}</div>`
- Logical expressions: `show && <Content />`
- Arrow function bodies: `() => <Component />`
- Parenthesized expressions: `(<Component />)`

#### 10.6.2 JSX Recognition Patterns

When `<` is encountered in expression context, it starts JSX if followed by:
1. **Capital letter**: `<Component` → JSX component
2. **HTML tag name**: `<div`, `<span`, `<input` → JSX element
3. **Fragment**: `<>` → JSX fragment
4. **Closing tag**: `</` → JSX closing tag
5. **Qualified name**: `<Form.Input`, `<ui.Button` → Namespaced JSX

#### 10.6.3 Non-JSX Patterns

`<` is **NOT** JSX when:
1. **Primitive types**: `<string>`, `<number>` → Type assertion
2. **Spaced comparison**: `x < 5` → Less-than operator
3. **Generic calls**: `Array<T>(args)` → Generic function call
4. **Channel operations**: `<-` → Channel receive
5. **In type context**: After `:`, `extends`, `implements` → Generic type

#### 10.6.4 Disambiguation Algorithm

1. **Context Check**: Is `<` in expression context?
2. **Pattern Match**: Does following text match JSX patterns?
3. **Lookahead**: For ambiguous cases, check for JSX continuation (`>`, `/>`, attributes)
4. **Fallback**: If not JSX, apply generic/comparison rules

#### 10.6.5 Type Assertions in JSX

In JSX contexts, use `as` syntax instead of angle brackets:
```javascript
// Preferred in JSX contexts
<input ref={ref as React.RefObject<HTMLInputElement>} />

// Avoid (ambiguous with JSX)
<input ref={<React.RefObject<HTMLInputElement>>ref} />
```

### 10.7 JSX Semantics

JSX expressions compile to function calls:

```javascript
// JSX source
<Button size="large" onClick={handleClick}>
  Click me
</Button>

// Compiles to (React.createElement style)
React.createElement(Button, 
  { size: "large", onClick: handleClick },
  "Click me"
)

// Or with pragma (e.g., /** @jsx h */)
h(Button, { size: "large", onClick: handleClick }, "Click me")
```

Fragments compile to:
```javascript
<>content</> → React.Fragment or Fragment
```

### 10.8 JSX Spread Semantics

```javascript
// Props spread
<Component {...props} extra="value" />
→ createElement(Component, { ...props, extra: "value" })

// Children spread  
<Component>{...items}</Component>
→ createElement(Component, null, ...items)
```

### 10.9 JSX Special Attributes

Certain attributes have special handling:
- `className` (React) and `class` (others) both accepted
- `htmlFor` (React) and `for` (others) both accepted
- `key` and `ref` are reserved for framework use
- Event handlers follow framework conventions (`onClick`, `on:click`, etc.)
- Style can be object (`style={{color: 'red'}}`) or string (`style="color: red"`)

---

## 11. Keywords

These are reserved as keywords unless written in backticks per §2.3.1.

```
await break case catch class const continue default defer def do done
elif else end enum export extends false fi final for fun function go if
import in interface let loop match new null package return struct switch
then this throw trait true try type until unsafe using var when while
```

**Soft keywords:** `regex`, `div`, `fallthrough`. They are ordinary identifiers except where explicitly consumed (§2.5, §8.1).

**Context-sensitive keywords:** `type` is treated as a declaration keyword only when it appears at the start of a statement followed by an identifier. In all other contexts (e.g., after `let`, `const`, `var`, as a variable name in expressions), `type` is treated as a regular identifier.

---

## 12. Module system

```
ImportStmt ::= ("import" | "require" | "using" | "#include") Path [ "as" Identifier ] ;
```

* `#include` is allowed only at top level and performs **textual inclusion before tokenisation**.
* No macro processing; `#define`, `#if` and similar are not recognized.
* All import spellings feed the same resolver.
* Cyclic imports raise run-time `ImportCycle`.

---

## 13. Execution semantics

### 13.1 Memory

Single shared heap with tracing GC. Object header includes a type tag and v-table pointer.
Deterministic destruction occurs via `finally` paths from `using` / `defer` / RAII.

### 13.2 Truthiness

A value is false only if it is **exactly** one of:
`false`, `0`, `0.0`, `0n` (bigint zero), `null` (alias `undefined`), empty `string`, empty `bytes`, empty array, empty map, `NaN`.
All other values are true.

### 13.3 Arithmetic and overflow

* Mixed numeric operands widen to a common supertype; integers may widen to `f64`.
* Signed overflow yields a `bigint` result.
* Unsigned overflow wraps modulo 2ⁿ.
* `+` concatenates if either operand is `string`.

### 13.4 Concurrency

`go expr` starts a **green task** in the same address space. If `expr` is a function call or lambda, its result is ignored; a `Future<any>` handle is returned.
`chan<T>` is a built-in generic channel type. `make(chan<T>, capacity=0)` creates a channel. Channels are FIFO. Send blocks when the buffer is full (or unbuffered).
Send: `ch <- v`. Receive: `<- ch`. Communication over a channel establishes happens-before.

Awaiting a future uses `await` with JavaScript semantics.

### 13.5 Error model

Any statement may raise `Error`.
`try { … } catch (e) { … }`, `except`, and `rescue` are synonyms.
Uncaught errors unwind to the task boundary, terminate the task, and the parent task receives a `JoinError`.

---

## 14. Directives and attributes

* Directive comments begin with `// @` or `# @`, are recognized during parse, and are then dropped. Reserved: `@generics`, `@nogenerics`, `@jsx`, `@jsxFrag`, `@jsxImportSource`, `@lint-disable` (others may be added). Directives apply to the **next statement only** unless stated.
* Attributes `[#[name]]` or `@annotation` may adorn declarations; preserved in the AST; ignored by core semantics.

---

## 15. Tooling guarantees

1. **Formatter** does not change chosen string delimiters and inserts semicolons only where MASI would.
2. **LSP rename** operates on the NFC-normalized identifier. `$foo`, `foo`, and backtick forms are the same symbol.
3. **Debugger** may keep dual parses where ambiguity exists and binds breakpoints to character spans.

---

## 16. Compliance definition

A compiler is PolyScript-1.0 compliant if:

1. It accepts every program valid under §§2–11.
2. It rejects any program violating a compile-time error.
3. Its run-time behaviour (I/O, exceptions, global state) matches §§6–13.

Performance, code layout, and optimization are out of scope.

---

## 17. Annex A — Illustrative files (all valid)

```polyscript
#!/usr/bin/env polyscript
// Mixed Fibonacci
def fib(n:int) -> int:
    if n <= 1:
        return n
    fi
    return fib(n-1) + fib(n-2)
end
```

```polyscript
# Quicksort monster: Py list-comp + JS spread + Go :=
function quicksort(arr){
    if len(arr) < 2:
        return arr
    pivot := arr[0]
    left  = [x for x in arr[1:] if x <= pivot]
    var right = arr.slice(1).filter(x => x > pivot)
    return [...quicksort(left), pivot] + quicksort(right)
}
```

```polyscript
// React component with JSX and TypeScript annotations
interface Props {
    title: string
    items: string[]
    onSelect?: (item: string) => void
}

function TodoList({ title, items, onSelect }: Props) {
    const [filter, setFilter] = useState("")
    
    const filtered = items.filter(item => 
        item.toLowerCase().includes(filter.toLowerCase())
    )
    
    return (
        <div className="todo-list">
            <h2>{title}</h2>
            <input 
                type="text"
                value={filter}
                onChange={e => setFilter(e.target.value)}
                placeholder="Filter items..."
            />
            <ul>
                {filtered.map(item => (
                    <li key={item} onClick={() => onSelect?.(item)}>
                        {item}
                    </li>
                ))}
            </ul>
            {filtered.length === 0 && <p>No items found</p>}
        </div>
    )
}

// JSX with spread props and fragments
const App = () => {
    const props = { size: "large", variant: "primary" }
    
    return <>
        <Button {...props}>Click me</Button>
        <Container>
            <TodoList 
                title="My Tasks" 
                items={["Learn PolyScript", "Build something"]}
            />
        </Container>
    </>
}
```

Each file also passes unmodified through its donor-language compiler (with undeclared features stubbed), preserving the **"every donor program stays valid"** pledge.



Use **ES2022 JavaScript as the primary target**, with a small runtime library. Emit ES modules that run on Node 22+, Deno, Bun, and the browser.

### Why JS

* Ubiquitous GC, BigInt, RegExp, async/await.
* Easy embedding and shebang support.
* Matches many donor surface features.
* Channels and green tasks can be modeled with promises and a cooperative scheduler.

### Minimal plan

1. **Front end:** Parser → core AST (per §4) → simple SSA-ish IR.
2. **Lowering:** Desugar all donor forms to the core.
3. **Codegen:** IR → ES module JS.
4. **Runtime:** `poly_runtime.ts` providing numbers, channels, tasks, RAII, equality, exceptions.
5. **Tooling:** Formatter, LSP, sourcemaps.

### Key mappings

* **Types:**

  * `bool → boolean`, `string → string`, `bytes → Uint8Array`, `char → string(1)`, `any → unknown`.
  * Integers (`i8…i64`, `u8…u64`) as **tagged BigInt** with width metadata. Floats as `number` (`f32`, `f64`). `bigint → BigInt`.
  * Literal narrowing handled in the front end; runtime ops enforce width.
* **Numeric ops (§12.3):**

  * Implement `add_i32`, `add_u32`, etc. Signed overflow: promote result to `BigInt` wrapper. Unsigned overflow: mask with `& ((1n<<n)-1n)`. Mixed numeric: widen per spec.
* **Equality (§9.1):**

  * Provide `looseEq`, `strictEq`, and `regexMatch` for `=~`. Do not use JS `==`.
* **Strings and templates (§2.4):**

  * Lower to JS template literals plus helper `fmt()` for `f` and `c` prefixes. `b` produces `Uint8Array`. `r` disables escapes in the front end.
* **Regex vs division (§2.5):**

  * Lexer implements **Prev-can-end-expression** rule; optional `regex/ div` soft override honored.
* **MASI semicolons (§2.7):**

  * Emit real `;` in JS at all `VirtualSemi` points.
* **Modules (§11):**

  * All imports lower to ES `import`. `#include` handled as pre-tokenization textual include.
* **Blocks (§3):**

  * Indent and keyword stacks resolved in parser. JS receives only `{}` blocks.
* **Control flow:**

  * `switch/match` lowers to JS `switch` or a table. `fallthrough` comment/token gates explicit fall-through.
* **RAII / `using` / `defer` (§8.2):**

  * Compile to `try/finally`. Define `Symbol.for("poly.dispose")` and call it on scope exit; also accept `.close()` if present.
* **Concurrency (§12.4):**

  * `go f()` returns `PolyFuture` wrapping a `Promise<void|any>`.
  * `chan<T>` implemented as an async FIFO with backpressure.
  * `send (ch <- v)` returns a promise that resolves when enqueued; receiver awaits `<- ch`.
  * Scheduler uses microtasks; blocking is simulated by awaiting internal promises.
* **Errors (§12.5):**

  * Map `Error` to JS `Error`. Task boundary propagation via rejected promises.

### Reference artifacts

* **`poly_runtime.ts` surface:**

  ```ts
  export type PolyInt = { kind:"i32"|...|"u64", v: bigint };
  export function add(a:any,b:any):any;           // dispatch by tags
  export function looseEq(a:any,b:any):boolean;
  export class Chan<T> { send(v:T):Promise<void>; recv():Promise<T>; close():void; }
  export function go<F extends (...a:any)=>any>(f: F, ...args: Parameters<F>): PolyFuture<ReturnType<F>>;
  export type PolyFuture<T> = Promise<T>;
  export const disposeSym = Symbol.for("poly.dispose");
  ```
* **CLI:** transpile `.poly` to `.mjs` and preserve shebang.

### Alternatives (short)

| Target          | Pros                          | Cons                                            | Use when                       |
| --------------- | ----------------------------- | ----------------------------------------------- | ------------------------------ |
| **Python 3.12** | Big ints, asyncio, easy C FFI | Browser story weak, perf                        | Prototyping interpreter        |
| **Go**          | Native goroutines/channels    | Dynamic features painful, BigInt lib            | AOT server builds later        |
| **WASM GC**     | Fast, portable                | Channels and scheduler needed, toolchain weight | Phase 2 backend fed by same IR |
| **JVM/.NET**    | Mature JIT, tooling           | Startup, interop friction, regex quirks         | Enterprise embedding           |

### Recommendation

* **Phase 1:** Implement a **TypeScript interpreter** over the core AST for spec correctness.
* **Phase 2:** Add JS codegen with `poly_runtime.ts`. Ship Node and browser targets.
* **Phase 3:** Optional WASM GC backend for hot paths, reuse the same runtime surface for channels and tasks.

This path keeps the spec intact, delivers fast, and stays portable.


Build a **hand-rolled recursive-descent parser** with a **Pratt expression parser**, fed by a lexer that preserves line/indent metadata and token adjacency. Emit the **core AST (§4)** only.

# Plan

## 1) Token stream contract

* Longest-match operators. Track:

  * `kind`, `lexeme`, `start`, `end`, `line`, `col`.
  * `virtualSemi: boolean`.
  * `wsBefore: boolean`, `wsAfter: boolean` for adjacency tests.
  * `indentCol` on the first token of each non-blank line.
  * `newline: boolean` for line starts.
* Emit `TemplateLiteral` as JS-style segments.
* Emit `RegexLiteral` using Prev-can-end-expr rule. Honor soft overrides `regex/ div` by peeking the **immediately adjacent** previous identifier and `wsAfter===false`.
* Implement MASI: at each linebreak, insert `VirtualSemi` unless rule 1/2/3 says to suppress. Rule 3 uses the next non-blank line’s indent (precomputed).
* Keep **directive tokens** for `// @generics`, `// @nogenerics`, `// @lint-disable` with `appliesToNextStmt=true`.
* `#include` is resolved pre-lexing (textual include with cycle guard).

## 2) Parser skeleton

* Stateful stacks:

  * `braceDepth`.
  * `indentStack: number[]` (columns).
  * `kwStack: ("do"|"case"|"begin"|"if"|"for"|"while"|"function")[]`.
* Statement boundary tokens: `; | VirtualSemi | } | end/fi/done/esac`.
* Rollback checkpoints for tentative parses (generics and arrow/lambda).

```ts
function parseProgram(): Program {
  const ds: (Decl|Stmt)[] = [];
  while (!at(Eof)) ds.push(parseTopLevel());
  return {kind:"Program", body:ds, span:spanFrom(ds)};
}

function parseTopLevel(): Decl|Stmt {
  consumeDirectivesForNextStmt();
  if (isDeclStart()) return parseDecl();
  return parseStmt();
}
```

## 3) Blocks and indentation

* **Indent blocks** open only if a header line ends with `:` and the **next non-blank** line has `indentCol` greater than the previous non-blank line. On open: push indent; parse statements until out-dent. On out-dent: pop; error if levels cross.
* **Keyword blocks**: on `do|case|begin|if(for Ruby)|while(Ruby)|function(Bash)` push kw; require matching `done|esac|end|fi`. Error if mismatched or crossed with other stacks.
* **Brace blocks**: normal `{}` with nesting.
* Crossing any stacks is a compile-time error.

## 4) Declarations and statements

* Accept all donor spellings but **lower immediately** to core nodes.
* `ShortDecl` uses `:=`. `Reassign` uses `:=:` with scope check deferred to name-binding pass.

```ts
function parseStmt(): Stmt {
  if (match("if")) return parseIf();
  if (match("switch","match","case")) return parseSwitch();
  if (match("for","while","until")) return parseLoopLike();
  if (match("try")) return parseTryLike();
  if (match("using","with")) return parseUsing();
  if (match("defer")) return parseDefer();
  // echo/print, break/continue/return …
  return parseExprStmt();
}
```

## 5) Expressions: Pratt parser

* Implement precedence levels 1–18 from §9 with right-associativity for `**` and assignments.
* Postfix chain (`call/index/member/?.`) at highest precedence.
* Ternary handled as a Pratt infix op between `??` and assignments.
* `new` with args at precedence 2.
* `=~` compiles as a binary op.
* Optional chaining `?.` is postfix in the chain stage.

```ts
function parseExpr(p = 0): Expr {
  let left = parsePrefix();
  while (true) {
    const op = peekInfix();
    if (!op || prec(op) < p) break;
    if (isRightAssoc(op)) {
      const q = prec(op);
      next(); const right = parseExpr(q);
      left = infixNode(op,left,right);
    } else {
      const q = prec(op)+1;
      next(); const right = parseExpr(q);
      left = infixNode(op,left,right);
    }
  }
  return left;
}
```

## 6) Generics `<` disambiguation

* Trigger only when `Identifier` is **immediately** followed by `<` with `wsAfter(identifier)===false`.
* Tentatively parse a **type-arg list** with angle-depth. Accept `>` `>>` `>>>` by **splitting** tokens inside this mode.
* Commit only if the next token is one of:
  `(` `[` `{` `>` `>>` `>>>` `:` `extends` `implements` `where` or a type use site.
* Otherwise rollback and treat `<` as an operator.
* Respect `@generics` or `@nogenerics` directive for the next statement.

## 7) Identifiers

* Backticks: if a `TemplateLiteral` with zero `${}` and content matches `^[A-Za-z_]\w*$` **and** we are in an **identifier position** (decl name, label, non-computed member, `import … as …`), reinterpret as `Identifier`. Else keep as string.
* `$foo` tokens normalize to `Identifier("foo")` at AST build time, preserving the original spelling for tooling.

## 8) Literals

* Strings: support `' " ''' """ ` \`\`, `%q/%w/%i/%s/%r`, `$'…'`, `$"…", @$"…", @$'…'`. Emit one `StringLiteral` or `TemplateLiteral` with flags `{raw,bytes,format,const}` (`r,b,f,c`) and pre-split template segments for back-end formatting.
* Numbers: store raw, base, separators stripped, and suffix (`n` or width). Narrowing is a later pass.
* Regex: `RegexLiteral` with flags; linebreaks rejected unless escaped.

## 9) JSX parsing

* **JSX disambiguation**: Pattern-based recognition in expression contexts:
  * Check if `<` is in valid JSX context (after `=`, `return`, `=>`, `?`, `:`, etc.)
  * Apply JSX patterns from §10.6.2: Capital letter, HTML tag, `>`, `/`, qualified names
  * Use lookahead for ambiguous cases (check for `>`, `/>`, attributes)
  * Fallback to comparison/generic rules if not JSX

* **JSX expression contexts**: JSX valid after:
  * Assignment operators: `=`, `:=`
  * Return statements: `return`
  * Ternary operators: `?`, `:`
  * Logical operators: `&&`, `||`
  * Array/object literals: `[`, `{`, `,`
  * Function calls: `(`, `,`
  * Parentheses: `(`
  * Arrow functions: `=>`

* **JSX AST construction**:
  * Self-closing: `<Component />` → `JSXElement` with `selfClosing:true`
  * Container: Match opening/closing tags, collect children
  * Fragments: `<>...</>` → `JSXFragment`  
  * Spread props: `{...expr}` in attributes or children
  * Expression containers: `{expr}` in JSX children or attribute values

* **JSX lexer integration**: Parser drives JSX detection, lexer follows:
  * Parser determines JSX context and informs lexer
  * Lexer switches modes based on parser signals
  * Track JSX depth for proper nesting and virtual semicolon suppression

## 10) Directives and soft keywords

* Keep a small parser state: `nextStmtGenericMode: "on"|"off"|"auto"`. Reset after consuming a statement.
* `fallthrough`: accept only immediately before the **next** switch arm. Model as a boolean on the previous case arm.

## 11) Error handling

* Panic-mode recovery sets of likely followers:

  * After expression: `; | VirtualSemi | ) | ] | } | else | elif | catch | finally | end/fi/done/esac`.
  * After header `:` when next line is not indented: report and synthesize a one-statement block.
* Produce spans and one-line diagnostics with quick-fix hints (e.g., “add `div` before `/` for division”).

## 12) AST shape (core, minimal)

```ts
type Id = { kind:"Ident", name:string, span:Span };
type TypeNode =
  | {kind:"SimpleType", id:Id}
  | {kind:"NullableType", inner:TypeNode}
  | {kind:"UnionType", types:TypeNode[]}
  | {kind:"GenericType", base:Id, args:TypeNode[]}
  | {kind:"FuncType", params:TypeNode[], ret:TypeNode}
  | {kind:"IndexedAccessType", object:TypeNode, index:string};

type Expr =
  | {kind:"LitNum", raw:string, suffix?:string, span:Span}
  | {kind:"LitStr", parts:StrPart[], flags:{r?:1,b?:1,f?:1,c?:1}, span:Span}
  | {kind:"LitRegex", pattern:string, flags:string, span:Span}
  | {kind:"Ident", name:string, span:Span}
  | {kind:"Call", callee:Expr, args:Expr[], span:Span}
  | {kind:"Index", obj:Expr, idx:Expr, span:Span}
  | {kind:"Member", obj:Expr, prop:Id, optional?:boolean, span:Span}
  | {kind:"Unary", op:string, arg:Expr, prefix:boolean, span:Span}
  | {kind:"Binary", op:string, left:Expr, right:Expr, span:Span}
  | {kind:"Assign", op:string, left:Expr, right:Expr, span:Span}
  | {kind:"Lambda", params:Param[], ret?:TypeNode, body:Block|Expr, async?:boolean, unsafe?:boolean, span:Span}
  | {kind:"JSXElement", tag:string|Id, attrs:JSXAttr[], children:JSXChild[], selfClosing:boolean, span:Span}
  | {kind:"JSXFragment", children:JSXChild[], span:Span};

type JSXAttr = 
  | {kind:"JSXAttr", name:string, value?:Expr|string, span:Span}
  | {kind:"JSXSpread", expr:Expr, span:Span};

type JSXChild =
  | {kind:"JSXText", text:string, span:Span}
  | {kind:"JSXExpr", expr:Expr, span:Span}
  | Expr; // JSXElement or JSXFragment

type Stmt =
  | {kind:"ExprStmt", expr:Expr, span:Span}
  | {kind:"If", arms:{test:Expr, body:Block}[], elseBody?:Block, span:Span}
  | {kind:"Loop", mode:"for"|"while"|"until"|"foreach"|"infinite", init?:Stmt, test?:Expr, step?:Stmt, body:Block, span:Span}
  | {kind:"Switch", scrut:Expr, cases:{pats:Expr[], body:Block, fallthrough?:boolean}[], defaultBody?:Block, span:Span}
  | {kind:"Try", body:Block, catches:{id?:Id, type?:TypeNode, body:Block}[], finallyBody?:Block, span:Span}
  | {kind:"Using", resource:Expr|Decl, body:Block, span:Span}
  | {kind:"Defer", body:Block|Expr, span:Span}
  | {kind:"Break", label?:Id, span:Span}
  | {kind:"Continue", label?:Id, span:Span}
  | {kind:"Return", values:Expr[], span:Span}
  | {kind:"Echo", values:Expr[], span:Span};

type Decl =
  | {kind:"Import", path:string, alias?:Id, span:Span}
  | {kind:"VarDecl", mut:boolean, names:Id[], type?:TypeNode, values?:Expr[], span:Span}
  | {kind:"ShortDecl", pairs:{name:Id, expr:Expr}[], span:Span}
  | {kind:"Reassign", name:Id, expr:Expr, span:Span}
  | {kind:"FuncDecl", name:Id, params:Param[], ret?:TypeNode, async?:boolean, unsafe?:boolean, body:Block, span:Span}
  | {kind:"TypeDecl", name:Id, def:TypeNode, span:Span};

type Block = { kind:"Block", stmts:(Decl|Stmt)[], span:Span };
type Param = { name:Id, type?:TypeNode, init?:Expr, span:Span };
```

## 13) Lowering hooks

* During AST build, normalize synonyms (`elseif→elif`, `except→catch`, etc.).
* Attach a `sourceName` vs `canonicalName` for identifiers to keep `$foo` and backticks intact for tooling.

## 14) Tests you must pass

* MASI edge cases across all three rules.
* Regex vs division at every precedence boundary; soft overrides.
* Backtick identifiers in all identifier positions and rejected in expression positions.
* Generics disambiguation with `<`, `<<`, `>>`, `>>>`, whitespace adjacency, and directives.
* All three block stacks, including crossing-stack error.
* Numeric literal suffix matrix and invalid underscore placement.
* `fallthrough` token and comment form.
* JSX elements (self-closing, nested, fragments) with proper tag matching.
* JSX attributes (spread, expressions, string literals).
* JSX vs generics disambiguation based on context.
* JSX text content with proper whitespace handling and entity escaping.
* Mixed JSX/TypeScript with generic components and type assertions.

This gives you a deterministic parser with rollback only where needed, a single AST that matches §4, and clean seams for later type checking and codegen.


