import { Token, TokenType } from './lexer';
import * as AST from './ast';
import { ParserCursor, ParseError } from './parser-cursor';
import * as JSX from './parselets/jsx';
import * as Types from './parselets/types';
import * as Functions from './parselets/functions';
import * as Imports from './parselets/imports';
import * as Literals from './parselets/literals';
import * as ClassDecl from './parselets/class-decl';
import * as ControlFlow from './parselets/control-flow';
import * as Blocks from './parselets/blocks';
import * as ExprPrefix from './parselets/expr-prefix';
import * as ExprPostfix from './parselets/expr-postfix';
import * as Decls from './parselets/declarations';
import { ParseletRegistry } from './parselet-registry';
import { classifyRustAnchor, scanRustItemAt, rustConstTypeSignal, RustItemKind } from './rust-item-scanner';

export { ParseError } from './parser-cursor';

export class Parser extends ParserCursor {
  public registry = new ParseletRegistry<Parser>();

  constructor(tokens: Token[], source?: string) {
    super(tokens, source);
    this.registerParselets();
  }

  private registerParselets(): void {
    // ---- Statement parselets (keyword already consumed via match()) ----
    this.registry
      .registerStatement("if", (p) => ControlFlow.parseIf(p))
      .registerStatement("do", (p) => ControlFlow.parseDoStatement(p))
      .registerStatement("for", (p) => ControlFlow.parseLoop(p))
      .registerStatement("while", (p) => ControlFlow.parseLoop(p))
      .registerStatement("until", (p) => ControlFlow.parseLoop(p))
      .registerStatement("loop", (p) => ControlFlow.parseLoop(p))
      .registerStatement("foreach", (p) => ControlFlow.parseForeach(p))
      .registerStatement("try", (p) => ControlFlow.parseTry(p))
      .registerStatement("with", (p) => ControlFlow.parseUsing(p))
      .registerStatement("defer", (p) => ControlFlow.parseDefer(p))
      .registerStatement("break", (p) => ControlFlow.parseBreak(p))
      .registerStatement("continue", (p) => ControlFlow.parseContinue(p))
      .registerStatement("return", (p) => ControlFlow.parseReturn(p))
      .registerStatement("assert", (p) => ControlFlow.parseAssert(p))
      .registerStatement("echo", (p) => ControlFlow.parseEcho(p))
      .registerStatement("print", (p) => ControlFlow.parseEcho(p))
      .registerStatement("throw", (p) => ControlFlow.parseThrow(p))
      .registerStatement("raise", (p) => ControlFlow.parseThrow(p))
      .registerStatement("go", (p) => ControlFlow.parseGo(p))
      .registerStatement("pass", (p) => ControlFlow.parsePass(p))
      .registerStatement("select", (p) => Blocks.parseSelectStatement(p))
      .registerStatement("begin", (p) => Blocks.parseBeginBlock(p));

    // ---- Declaration parselets (keyword already consumed via match()) ----
    this.registry
      .registerDeclaration("import", (p) => Imports.parseImport(p))
      .registerDeclaration("require", (p) => Imports.parseImport(p))
      .registerDeclaration("let", (p) => Decls.parseVarDecl(p))
      .registerDeclaration("var", (p) => Decls.parseVarDecl(p))
      .registerDeclaration("auto", (p) => Decls.parseVarDecl(p))
      .registerDeclaration("const", (p) => Decls.parseConstDecl(p))
      .registerDeclaration("final", (p) => Decls.parseConstDecl(p))
      .registerDeclaration("immutable", (p) => Decls.parseConstDecl(p))
      .registerDeclaration("class", (p) => ClassDecl.parseClassDecl(p))
      .registerDeclaration("interface", (p) => ClassDecl.parseInterfaceDecl(p))
      .registerDeclaration("trait", (p) => ClassDecl.parseInterfaceDecl(p))
      .registerDeclaration("enum", (p) => ClassDecl.parseEnumDecl(p))
      .registerDeclaration("package", (p) => Decls.parsePackageDecl(p))
      .registerDeclaration("export", (p) => Decls.parseExportDecl(p))
      .registerDeclaration("type", (p) => Decls.parseTypeDecl(p));
  }
  
  parse(): AST.Program {
    const body: (AST.Decl | AST.Stmt)[] = [];
    let iterations = 0;
    const maxIterations = Math.max(1000, this.tokens.length * 2); // Safety limit with minimum
    
    while (!this.isAtEnd()) {
      iterations++;
      if (iterations > maxIterations) {
        return {
          kind: "Program",
          body,
          runtimeDirective: this.fileRuntimeDirective,
          span: this.createSpan(0, Math.min(this.current, this.tokens.length - 1))
        };
      }
      
      const beforePos = this.current;
      
      try {
        const item = this.parseModuleItem();
        if (item) {
          body.push(item);
        } else if (!this.isAtEnd()) {
          if (this.current === beforePos) {
            this.advance();
          }
        }
      } catch (error) {
        if (error instanceof ParseError) {
          this.errors.push(error);
          this.synchronize();
        } else {
          throw error;
        }
      }
      
      if (this.current === beforePos && !this.isAtEnd()) {
        this.advance();
      }
    }
    
    return {
      kind: "Program",
      body,
      runtimeDirective: this.fileRuntimeDirective,
      span: this.createSpan(0, this.tokens.length - 1)
    };
  }

  /**
   * Parse one module-level item, applying the two-regime Rust architecture:
   *
   * Regime 2 (default): the fine-grained union grammar parses the item, so
   * expression-level Rust mixed into other languages keeps interleaving.
   *
   * Regime 1 (opaque item slicing): when the item starts at a definite-Rust
   * item anchor and the union grammar either fails, produces errors, or
   * yields a node of the wrong shape, the Rust raw scanner consumes exactly
   * one item VERBATIM into an opaque RustItem node. A few anchors that no
   * other donor language can produce at module level (`static X: T`,
   * `mod x {`, `macro_rules!`, `extern crate`, Rust-typed `const X: T`)
   * scan unconditionally because the union grammar mis-routes them silently.
   *
   * Anchor detection operates on the token stream — string/comment content
   * never reaches it, so `"fn main() {}"` inside a Python string is inert.
   */
  private parseModuleItem(): AST.Decl | AST.Stmt | null {
    const anchor = this.rustItemAnchor();
    if (!anchor) return this.parseTopLevel();

    const anchorToken = this.tokens[anchor.tokenIndex];
    const itemKind = classifyRustAnchor(this.sourceText!, anchorToken.start);
    if (!itemKind) return this.parseTopLevel();

    const checkpoint = this.current;
    const errCheckpoint = this.errors.length;
    const savedBrace = this.braceDepth;
    const savedIndent = [...this.indentStack];
    const savedKeyword = [...this.keywordStack];

    if (anchor.unconditional) {
      const item = this.scanRustItemNode(anchorToken);
      if (item) return item;
      return this.parseTopLevel();
    }

    let node: AST.Decl | AST.Stmt | null = null;
    let thrown: ParseError | undefined;
    try {
      node = this.parseTopLevel();
    } catch (error) {
      if (!(error instanceof ParseError)) throw error;
      thrown = error;
    }

    // A Rust where-clause after the union node means the union grammar
    // stopped mid-item (`fn f<T>(x: T) -> f64 where T: ...`): mis-sliced.
    const stoppedAtWhere = !thrown && this.peekPastVsemis()?.value === "where";

    if (!thrown && !stoppedAtWhere && this.errors.length === errCheckpoint && node &&
        this.unionShapeMatchesRustAnchor(itemKind, node) &&
        this.unionExtentMatchesScanner(itemKind, node, anchorToken)) {
      return node;
    }

    // The union grammar can't express this item — slice it opaquely.
    const unionPosition = this.current;
    this.current = checkpoint;
    this.braceDepth = savedBrace;
    this.indentStack = savedIndent;
    this.keywordStack = savedKeyword;
    const item = this.scanRustItemNode(anchorToken);
    if (item) {
      this.errors.length = errCheckpoint;
      return item;
    }

    // Scanner declined too — keep the union outcome.
    this.current = unionPosition;
    if (thrown) throw thrown;
    return node;
  }

  /**
   * Detect a Rust item anchor at the cursor (past virtual semicolons).
   * Returns the anchor token index, and whether the anchor is definite
   * enough to bypass the union grammar entirely.
   */
  private rustItemAnchor(): { tokenIndex: number; unconditional: boolean } | null {
    if (!this.sourceText) return null;
    let i = this.current;
    while (i < this.tokens.length && this.tokens[i].virtualSemi) i++;
    const t0 = this.tokens[i];
    if (!t0) return null;

    const value = (k: number) => this.tokens[i + k]?.value;
    const isIdent = (k: number) => {
      const t = this.tokens[i + k];
      return !!t && (t.type === TokenType.Identifier || t.type === TokenType.Keyword);
    };

    /** Classify the anchor starting at token offset k (after pub, if any). */
    const anchorAt = (k: number): { unconditional: boolean } | null => {
      switch (value(k)) {
        case "fn":
          return isIdent(k + 1) ? { unconditional: false } : null;
        case "async":
        case "unsafe": {
          let j = k;
          while (value(j) === "async" || value(j) === "unsafe" || value(j) === "const") j++;
          if (value(j) === "fn" && isIdent(j + 1)) return { unconditional: false };
          // `unsafe impl ...` / `unsafe trait ...` — Rust-only item heads.
          // Without an anchor the union grammar silently parses them as
          // expression statements (mis-slice when a where-clause follows).
          if (value(k) === "unsafe" && value(k + 1) === "impl" &&
              (isIdent(k + 2) || value(k + 2) === "<")) {
            return { unconditional: false };
          }
          if (value(k) === "unsafe" && value(k + 1) === "trait" && isIdent(k + 2)) {
            return { unconditional: false };
          }
          return null;
        }
        case "const": {
          if (value(k + 1) === "fn" && isIdent(k + 2)) return { unconditional: false };
          if (isIdent(k + 1) && value(k + 2) === ":") {
            // `pub const NAME: ...` — `pub` is Rust-only syntax (TS has no
            // pub), so the anchor is unconditional regardless of the type.
            if (k > 0) return { unconditional: true };
            // Bare `const NAME: Type =` — unconditional only when the
            // declared type carries a Rust-only signal (or the initializer
            // contains a `::` path outside strings), so TS
            // `const x: number = 5` stays in the union grammar. The texts
            // are taken from the RAW SOURCE: the union lexer mangles
            // lifetimes (`&'static [...]` lexes as a string literal), so
            // token values are unreliable in this position.
            if (this.rawConstRustSignal(i + k + 2)) {
              return { unconditional: true };
            }
          }
          return null;
        }
        case "static": {
          let j = k + 1;
          if (value(j) === "mut") j++;
          return isIdent(j) && value(j + 1) === ":" ? { unconditional: true } : null;
        }
        case "use":
          // `use path::...;` and the prefix-less brace group `use { a::b,
          // c::d };` (the union import grammar only knows the former).
          return isIdent(k + 1) || value(k + 1) === "{"
            ? { unconditional: false } : null;
        case "struct":
        case "enum":
        case "trait":
        case "type":
          return isIdent(k + 1) ? { unconditional: false } : null;
        case "union":
          // `union Name {` / `union Name<` only — `union` is a plausible
          // identifier in other languages.
          return isIdent(k + 1) && (value(k + 2) === "{" || value(k + 2) === "<")
            ? { unconditional: true } : null;
        case "impl":
          return isIdent(k + 1) || value(k + 1) === "<" ? { unconditional: false } : null;
        case "mod":
          return isIdent(k + 1) && (value(k + 2) === "{" || value(k + 2) === ";")
            ? { unconditional: true } : null;
        case "macro_rules":
          return value(k + 1) === "!" ? { unconditional: true } : null;
        case "extern":
          return value(k + 1) === "crate" ? { unconditional: true } : null;
        default:
          return null;
      }
    };

    if (t0.value === "pub") {
      let k = 1;
      if (value(k) === "(") {
        let depth = 0;
        while (i + k < this.tokens.length) {
          if (value(k) === "(") depth++;
          else if (value(k) === ")") { depth--; if (depth === 0) { k++; break; } }
          k++;
          if (k > 12) return null;
        }
      }
      const inner = anchorAt(k);
      return inner ? { tokenIndex: i, ...inner } : null;
    }

    const direct = anchorAt(0);
    if (direct) return { tokenIndex: i, ...direct };

    // Module-level macro invocation: `ident!` / `path::ident!` immediately
    // followed by an opening delimiter (pin_project!{..}, cfg_if!{..},
    // delegate_all!(..);). Token-level — string/comment content never gets
    // here — and the first segment must be a plain Identifier, so union
    // statement keywords (assert/print/...) stay in the union grammar. The
    // anchor is unconditional but the scanner is strict (`()`/`[]` forms
    // REQUIRE the trailing `;`), so expression-position macro calls such as
    // `panic!("boom")` without a semicolon still fall back to union parsing.
    if (t0.type === TokenType.Identifier) {
      let j = 0;
      while (isIdent(j) && value(j + 1) === "::") j += 2;
      if (isIdent(j) && value(j + 1) === "!") {
        const delim = value(j + 2);
        if (delim === "{" || delim === "(" || delim === "[") {
          return { tokenIndex: i, unconditional: true };
        }
      }
    }
    return null;
  }

  /**
   * Definite-Rust signal for a bare module-level `const NAME : Type = ...;`
   * whose `:` is the token at `colonIndex`. True when the declared type
   * carries a Rust-only signal (rustConstTypeSignal) or the initializer
   * contains a `::` path outside string content (`StateID::ZERO`,
   * `GeneralPurpose::new(...)`). Operates on the RAW SOURCE because the
   * union lexer reads lifetimes (`&'static`) as string starts.
   */
  private rawConstRustSignal(colonIndex: number): boolean {
    const src = this.sourceText;
    const colon = this.tokens[colonIndex];
    if (!src || !colon) return false;
    const from = colon.start + 1;
    const cap = Math.min(src.length, from + 600);
    let depth = 0; // ([{ nesting — `[u64; 1]` array types contain `;`
    let stripped = ""; // source text with string contents removed
    let typeEnd = -1;
    let p = from;
    while (p < cap) {
      const c = src[p];
      if (c === '"') {
        p++;
        while (p < cap && src[p] !== '"') p += src[p] === "\\" ? 2 : 1;
        p++;
        stripped += " ";
        continue;
      }
      if (c === "'") {
        if (/[A-Za-z_]/.test(src[p + 1] || "") && src[p + 2] !== "'") {
          // Lifetime ('static, 'a) — keep it visible for the type signal.
          stripped += "'";
          p++;
          while (p < cap && /[A-Za-z0-9_]/.test(src[p])) { stripped += src[p]; p++; }
          continue;
        }
        // Char literal or single-quoted string — skip its content so a
        // TS `const sep: string = '::';` cannot fake a Rust path.
        p++;
        while (p < cap && src[p] !== "'") p += src[p] === "\\" ? 2 : 1;
        p++;
        stripped += " ";
        continue;
      }
      if (c === "(" || c === "[" || c === "{") depth++;
      else if (c === ")" || c === "]" || c === "}") { if (depth > 0) depth--; }
      else if (c === "=" && depth === 0 && typeEnd === -1) {
        // `==`, `=>` never sit in type position; a bare `=` ends the type.
        if (src[p + 1] !== "=" && src[p + 1] !== ">") {
          typeEnd = stripped.length;
          if (rustConstTypeSignal(stripped)) return true;
        }
      } else if (c === ";" && depth === 0) {
        if (typeEnd === -1) return false; // no initializer — not a Rust const
        return stripped.slice(typeEnd).includes("::");
      } else if (c === "\n" && typeEnd === -1 && depth === 0) {
        return false; // type position never spans a bare newline
      }
      stripped += c;
      p++;
    }
    return false;
  }

  /**
   * For brace-delimited Rust-only item kinds, the union grammar's node must
   * cover exactly the same source extent the raw scanner computes — anything
   * else is a silent mis-slice (e.g. an impl body the union parser ran
   * through). `fn` is exempt: poly-style fns (JSX/python bodies) legitimately
   * diverge from Rust brace counting, and use/static/type are `;`-terminated.
   */
  private unionExtentMatchesScanner(
    kind: RustItemKind,
    node: AST.Decl | AST.Stmt,
    anchorToken: Token,
  ): boolean {
    const braceKind = kind === "impl" || kind === "trait" || kind === "struct" ||
        kind === "enum" || kind === "union" || kind === "mod";
    if (!braceKind && kind !== "fn") return true;
    const src = this.sourceText;
    if (!src) return true;
    const scanned = scanRustItemAt(src, anchorToken.start);
    if (!scanned) return true; // scanner has no opinion — keep the union node

    if (kind === "fn") {
      // Poly-style fns (JSX/Python bodies) legitimately diverge from Rust
      // brace counting, so fn extents are normally exempt. But when the fn
      // SIGNATURE carries Rust-only syntax — a lifetime or a where clause —
      // the union parse is unreliable (the union lexer reads `<'_>` as the
      // start of a string literal and can swallow the rest of the line, so
      // the union fn body silently absorbs the following items): require
      // the extents to agree. The signature is taken from the anchor (past
      // doc comments, whose apostrophes would false-positive) up to the
      // body brace.
      const anchorRel = scanned.anchorStart - scanned.start;
      const bodyRel = scanned.text.indexOf("{", anchorRel);
      const sig = scanned.text.slice(anchorRel, bodyRel === -1 ? undefined : bodyRel);
      const rustOnlySig = /'(?:_|[A-Za-z]\w*)\b(?!')/.test(sig) || /\bwhere\b/.test(sig);
      if (!rustOnlySig) return true;
    }

    const lo = Math.min(scanned.end, node.span.end);
    const hi = Math.max(scanned.end, node.span.end);
    return /^[\s;]*$/.test(src.slice(lo, hi));
  }

  /** Does the union grammar's parse result fit what the anchor promises? */
  private unionShapeMatchesRustAnchor(kind: RustItemKind, node: AST.Decl | AST.Stmt): boolean {
    switch (kind) {
      case "fn": return node.kind === "FuncDecl";
      case "use": return node.kind === "Import" || node.kind === "ImportDecl" || node.kind === "GroupedImport";
      case "struct": case "type": return node.kind === "TypeDecl";
      case "enum": return node.kind === "EnumDecl";
      case "trait": return node.kind === "InterfaceDecl";
      case "impl": return node.kind === "ImplDecl";
      default: return false;
    }
  }

  /**
   * Regime 1: consume exactly one Rust item verbatim via the raw scanner,
   * skipping every token the item covers.
   */
  private scanRustItemNode(anchorToken: Token): AST.RustItem | null {
    const src = this.sourceText;
    if (!src) return null;
    const scanned = scanRustItemAt(src, anchorToken.start);
    if (!scanned) return null;

    while (!this.isAtEnd() && this.peek().virtualSemi) this.advance();
    while (!this.isAtEnd() && this.peek().start < scanned.end) this.advance();

    return {
      kind: "RustItem",
      itemKind: scanned.itemKind,
      text: scanned.text,
      ...(scanned.name ? { name: scanned.name } : {}),
      fns: scanned.fns,
      bindings: scanned.bindings,
      span: {
        start: scanned.start,
        end: scanned.end,
        line: anchorToken.line,
        column: anchorToken.column,
      },
    };
  }

  public parseTopLevel(): AST.Decl | AST.Stmt | null {
    this.consumeDirectives();
    
    // Skip virtual semicolons at top level
    let vsCount = 0;
    while (this.peek().virtualSemi) {
      this.advance();
      vsCount++;
      if (vsCount > 100) {
        return null;
      }
    }
    
    if (this.isAtEnd()) return null;

    // Handle closing braces
    // Note: braceDepth only tracks {} blocks (if/for/while/function bodies)
    // It does NOT track braces for classes, interfaces, object literals, etc.
    if (this.check("}")) {
      if (this.braceDepth > 0) {
        // We're inside a statement block, let parseBlock() handle it
        return null;
      } else {
        // This } belongs to a class/interface/object literal/etc
        // It should have been consumed already by the appropriate parser.
        // If we see it here, it's an extra/unmatched brace - skip it and continue
        this.advance();
        return this.parseTopLevel(); // Try to parse the next item
      }
    }
    
    // Check for decorators (@decorator syntax)
    if (this.check("@")) {
      const decorators: AST.Expr[] = [];
      const decoratorStart = this.current;
      while (this.check("@")) {
        this.advance(); // consume @
        
        // Parse decorator expression (function call or identifier)
        const name = this.parseIdentifier();
        
        // Allow function calls on the decorator: @deprecated("message")
        const decorator = this.parsePostfix(name);
        
        decorators.push(decorator);
        
        // Skip virtual semicolons between decorators
        while (this.peek().virtualSemi) {
          this.advance();
        }
      }
      
      this.skipJavaDeclarationModifiers();

      // After decorators, we expect a declaration (function or class)
      // Check for class first since Python can have 'def' inside class
      if (this.match("class")) {
        const cls = ClassDecl.parseClassDecl(this, decorators);
        return cls;
      } else if (this.match("async", "unsafe")) {
        // Handle async/unsafe before function
        const isAsync = this.previous()?.value === "async";
        const isUnsafe = this.previous()?.value === "unsafe";
        
        if (this.match("def", "fun", "fn", "func", "function")) {
          const isGenerator = this.previous()?.value === "function" && this.match("*");
          const func = Functions.parseFuncDecl(this, isAsync, isUnsafe, isGenerator, decorators, decoratorStart);
          return func;
        }
      } else if (this.match("def", "fun", "fn", "func", "function")) {
        const isGenerator = this.previous()?.value === "function" && this.match("*");
        const func = Functions.parseFuncDecl(this, false, false, isGenerator, decorators, decoratorStart);
        return func;
      } else if (Types.isType(this)) {
        const isRetTypeFn = this.attempt(() => {
          this.parseType();
          if (this.peek().type === TokenType.Identifier) {
            this.advance();
            if (this.check("(")) return true;
          }
          return null;
        });
        if (isRetTypeFn) {
          return Functions.parseFuncDeclWithReturnTypeBefore(this, decorators, decoratorStart);
        }
      }
      
      // If we have decorators but no valid declaration follows, it's an error
      throw this.error(this.peek(), "Expected function or class declaration after decorators");
    }
    
    // Check for short declaration first (including destructuring)
    if (this.peek().type === TokenType.Identifier) {
      const checkpoint = this.current;
      this.advance();
      if (this.check(":=")) {
        this.current = checkpoint;
        return this.parseShortDecl();
      }
      this.current = checkpoint;
    } else if ((this.peek().value === "{" || this.peek().value === "[") && 
               this.peekAhead(":=")) {
      // Destructuring pattern with :=
      return this.parseDestructuringShortDecl();
    }
    
    if (this.isDeclStart()) {
      return this.parseDeclaration();
    }
    
    return this.parseStatement();
  }
  
  public isStatementKeyword(keyword: string): boolean {
    // These keywords can start a statement and should not be treated as identifiers
    return keyword === "if" || keyword === "while" || keyword === "for" ||
           keyword === "do" || keyword === "switch" || keyword === "try" ||
           keyword === "throw" || keyword === "return" || keyword === "break" ||
           keyword === "continue" || keyword === "case" || keyword === "default" ||
           keyword === "new" || keyword === "yield" || keyword === "await" ||
           keyword === "match" || keyword === "using" || keyword === "defer" ||
           keyword === "go" || keyword === "echo";
  }
  
  /** `pub` followed by a Rust item keyword starts a Rust declaration. */
  public isRustPubDeclStart(): boolean {
    const next = this.peekNext()?.value;
    return next === "fn" || next === "struct" || next === "enum" ||
           next === "async" || next === "use" || next === "const" ||
           next === "static" || next === "trait" || next === "impl" ||
           next === "type" || next === "mod";
  }

  public isDeclStart(): boolean {
    const type = this.peek().type;
    const value = this.peek().value;
    
    // Check for decorators (@decorator)
    if (value === "@") {
      return true;
    }

    // Rust visibility modifier before an item keyword
    if (value === "pub" && this.isRustPubDeclStart()) {
      return true;
    }
    
    // Check for impl blocks (Rust-style)
    if (value === "impl" && type === TokenType.Identifier) {
      return true;
    }
    
    // Special check for using - it's only a declaration if it's an import
    if (value === "using") {
      const next = this.peekNext();
      // It's a declaration if followed by string literal or identifier (but not assignment)
      return next?.type === TokenType.StringLiteral || 
             (next?.type === TokenType.Identifier && this.peekAt(2)?.value !== "=");
    }
    
    // Java-style modifiers before class: public class, abstract class, public abstract class, etc.
    if (value === "public" || value === "private" || value === "protected" || value === "abstract") {
      // Scan ahead through modifiers to find class/interface/enum
      for (let i = 1; i <= 5; i++) {
        const ahead = this.peekAt(i);
        if (!ahead) break;
        if (ahead.value === "class" || ahead.value === "interface" || ahead.value === "enum") return true;
        if (ahead.value !== "public" && ahead.value !== "private" && ahead.value !== "protected" &&
            ahead.value !== "abstract" && ahead.value !== "static" && ahead.value !== "final") break;
      }
    }

    // Check for async/unsafe followed by function declarations
    if (value === "async" || value === "unsafe") {
      const next = this.peekNext();
      return next?.value === "fn" || next?.value === "fun" || 
             next?.value === "function" || next?.value === "def" || next?.value === "func" ||
             next?.value === "async" || next?.value === "unsafe";
    }
    
    // Python-style from X import Y — only if 'import' appears soon after
    if (value === "from" && type === TokenType.Keyword) {
      // Look ahead: from <path> import ... — path can be deeply dotted
      for (let i = 1; i <= 30; i++) {
        const ahead = this.peekAt(i);
        if (!ahead || ahead.type === TokenType.EOF) break;
        if (ahead.value === "import") return true;
        // Stop if we see operators that aren't dots, or virtual semis
        if (ahead.virtualSemi) break;
        if (ahead.type === TokenType.Operator && ahead.value !== "." && ahead.value !== ".." && ahead.value !== "...") break;
      }
      return false;
    }

    // Special check for 'type' - only a declaration if followed by identifier (type alias)
    if (value === "type") {
      const next = this.peekNext();
      // It's a type declaration if the next token is an identifier
      // This allows 'type' to be used as a regular identifier in expressions
      return next?.type === TokenType.Identifier;
    }
    
    return (
      type === TokenType.Keyword && (
        value === "import" || value === "require" ||
        value === "let" || value === "var" || value === "auto" ||
        ((value === "fn" || value === "fun" || value === "function" || value === "def" || value === "func") && this.peekNext()?.value !== "." &&
         // func/function followed by ( must look like a declaration, not a call
         (this.peekNext()?.value !== "(" || this.peekNext()?.value === "(" && this.looksLikeFuncDecl())) ||
        value === "const" || value === "final" || value === "immutable" ||
        value === "class" || value === "struct" || value === "interface" ||
        (value === "trait" && this.peekNext()?.type === TokenType.Identifier) || value === "enum" ||
        value === "package" || value === "export"
      ) ||
      // Also check for 'impl' as an identifier (Rust-style impl blocks)
      (type === TokenType.Identifier && value === "impl") ||
      type === TokenType.Operator && value === "#" && 
      this.peekNext()?.type === TokenType.Identifier &&
      this.peekNext()?.value === "include"
    );
  }

  /** Check if func/function( starts a declaration (body follows closing paren). */
  private looksLikeFuncDecl(): boolean {
    // Scan past matched parens from peekNext (which is "(")
    let pos = this.current + 1; // start at (
    let depth = 0;
    while (pos < this.tokens.length) {
      const t = this.tokens[pos];
      if (t.value === "(") depth++;
      else if (t.value === ")") {
        depth--;
        if (depth === 0) {
          // Check what follows: {, :, ->, => all indicate declaration/expression, not call
          const after = this.tokens[pos + 1];
          if (!after) return false;
          if (after.value === "{" || after.value === ":" || after.value === "->" || after.value === "=>") return true;
          return false;
        }
      }
      pos++;
    }
    return false;
  }

  private skipJavaDeclarationModifiers(): void {
    while (this.check("public") || this.check("private") || this.check("protected") ||
           this.check("abstract") || this.check("static") || this.check("final")) {
      this.advance();
    }
  }
  
  public parseDeclaration(): AST.Decl {
    const keyword = this.peek().value;

    // ---- Complex multi-token disambiguation ----

    // Python-style: from module import names
    if (keyword === "from" && this.isDeclStart()) {
      return Imports.parseFromImport(this);
    }

    // using — import vs resource management
    if (keyword === "using") {
      const next = this.peekNext();
      if (next?.type === TokenType.StringLiteral ||
          (next?.type === TokenType.Identifier && this.peekAt(2)?.value !== "=")) {
        this.advance();
        return Imports.parseImport(this);
      }
      throw this.error(this.peek(), "Expected declaration");
    }

    // #include
    if (this.check("#") && this.peekNext()?.value === "include") {
      this.advance(); // #
      this.advance(); // include
      return Imports.parseImport(this);
    }

    // Java-style modifiers before class/interface: public class, public abstract class, etc.
    if (keyword === "public" || keyword === "private" || keyword === "protected" || keyword === "abstract") {
      // Skip all modifiers until we find class/interface/enum
      this.skipJavaDeclarationModifiers();
      if (this.match("class")) {
        return ClassDecl.parseClassDecl(this);
      }
      if (this.match("interface")) {
        return ClassDecl.parseInterfaceDecl(this);
      }
      if (this.match("enum")) {
        return ClassDecl.parseEnumDecl(this);
      }
      throw this.error(this.peek(), "Expected class, interface, or enum after modifiers");
    }

    // async/unsafe modifiers before function decl
    if (keyword === "async" || keyword === "unsafe") {
      let isAsync = false;
      let isUnsafe = false;
      if (this.match("async")) {
        isAsync = true;
        if (this.match("unsafe")) isUnsafe = true;
      } else if (this.match("unsafe")) {
        isUnsafe = true;
        if (this.match("async")) isAsync = true;
      }
      if (this.match("def", "fun", "fn", "func", "function")) {
        const funcKeyword = this.previous()?.value;
        const isGenerator = funcKeyword === "function" && this.match("*");
        return Functions.parseFuncDecl(this, isAsync, isUnsafe, isGenerator);
      }
      throw this.error(this.peek(), "Expected function declaration after async/unsafe");
    }

    // Rust visibility modifier: pub fn / pub struct / pub enum / pub use / ...
    if (keyword === "pub" && this.isRustPubDeclStart()) {
      this.advance(); // consume 'pub'
      return this.parseDeclaration();
    }

    // Rust-style struct declaration: struct Name { ... } / struct Name<T>(...)
    if (keyword === "struct" && this.peekNext()?.type === TokenType.Identifier) {
      this.advance(); // consume 'struct'
      return Decls.parseStructDecl(this);
    }

    // Function declarations (with generator support)
    // Don't match if followed by '.' — that's member access (e.g., def.value = ...)
    if ((this.check("def") || this.check("fun") || this.check("fn") || this.check("func") || this.check("function"))
        && this.peekNext()?.value !== ".") {
      this.advance();
      const isGenerator = this.previous()?.value === "function" && this.match("*");
      return Functions.parseFuncDecl(this, false, false, isGenerator);
    }

    // Return-type-before-name function declaration (e.g. `int main()`)
    if (Types.isType(this)) {
      const isRetTypeFn = this.attempt(() => {
        this.parseType();
        if (this.peek().type === TokenType.Identifier) {
          this.advance();
          if (this.check("(")) return true;
        }
        return null;
      });
      if (isRetTypeFn) {
        return Functions.parseFuncDeclWithReturnTypeBefore(this);
      }
    }

    // Rust-style impl blocks
    if (keyword === "impl" && this.peek().type === TokenType.Identifier) {
      return ClassDecl.parseImplBlock(this);
    }

    // ---- Registry-driven simple keyword dispatch ----
    const declParselet = this.registry.getDeclaration(keyword);
    if (declParselet) {
      this.advance(); // consume the keyword
      return declParselet(this);
    }

    // Short declaration (Go-style :=)
    if (this.peek().type === TokenType.Identifier) {
      const checkpoint = this.current;
      this.advance();
      if (this.match(":=")) {
        this.current = checkpoint;
        return this.parseShortDecl();
      }
      this.current = checkpoint;
    }

    throw this.error(this.peek(), "Expected declaration");
  }
  
  public parseStatement(): AST.Stmt {
    const keyword = this.peek().value;

    // ---- Complex multi-token disambiguation (cannot be registry-driven) ----

    // async for → for-await loop
    if (keyword === "async" && this.peekNext()?.value === "for") {
      this.advance(); // consume async
      this.advance(); // consume for
      const loop = ControlFlow.parseLoop(this);
      loop.await = true;
      return loop;
    }

    // Python async with -> context-manager statement with an async modifier.
    // The Using node must keep the async bit so manifest lowering can call
    // __aenter__/__aexit__ instead of sync context-manager methods.
    if (keyword === "async" && this.peekNext()?.value === "with") {
      this.advance(); // consume async
      this.advance(); // consume with
      const using = ControlFlow.parseUsing(this);
      using.async = true;
      return using;
    }

    // switch/match/when → parseSwitch
    if (this.check("switch")) {
      this.advance();
      return Blocks.parseSwitch(this) as AST.Stmt;
    }
    if (this.check("match") && !this.isAssignmentOp(this.peekAt(1)!) && this.peekAt(1)?.value !== "{" &&
        this.peekAt(1)?.value !== ")" && this.peekAt(1)?.value !== "," && this.peekAt(1)?.value !== ";" &&
        this.peekAt(1)?.value !== "&&" && this.peekAt(1)?.value !== "||" && this.peekAt(1)?.value !== "?" &&
        this.peekAt(1)?.value !== "]" && this.peekAt(1)?.value !== "[" && this.peekAt(1)?.value !== "!" &&
        this.peekAt(1)?.value !== "." &&
        this.peekAt(1)?.type !== TokenType.EOF) {
      this.advance();
      return Blocks.parseSwitch(this) as AST.Stmt;
    }
    if (keyword === "when" && (this.peekAt(1)?.value === "(" || this.peekAt(1)?.value === "{")) {
      // Don't trigger match if when(...) is followed by `do` (Elixir-style macro def)
      let triggerMatch = true;
      if (this.peekAt(1)?.value === "(") {
        let scanPos = this.current + 2, depth = 1;
        while (scanPos < this.tokens.length && depth > 0) {
          if (this.tokens[scanPos].value === "(") depth++;
          if (this.tokens[scanPos].value === ")") depth--;
          scanPos++;
        }
        while (scanPos < this.tokens.length && this.tokens[scanPos].virtualSemi) scanPos++;
        if (this.tokens[scanPos]?.value === "do") triggerMatch = false;
      }
      if (triggerMatch) {
        this.advance();
        return Blocks.parseSwitch(this) as AST.Stmt;
      }
    }

    // Bash case...in...esac
    if (keyword === "case") {
      if (!this.insideSwitch) {
        this.advance();
        return Blocks.parseCaseStatement(this);
      } else {
        const cp = this.current;
        this.advance();
        const expr = this.parsePrimary();
        if (this.check("in") || this.check("when")) {
          this.advance();
          return Blocks.parseCaseEsac(this, cp, expr);
        }
        this.current = cp;
      }
    }

    // using — resource management (not import)
    if (keyword === "using") {
      const next = this.peekNext();
      const nextNext = this.peekAt(2);
      if ((next?.type === TokenType.Identifier && nextNext?.value === "=") ||
          next?.value === "(") {
        this.advance();
        return ControlFlow.parseUsing(this);
      }
    }

    // ---- Registry-driven simple keyword dispatch ----
    const stmtParselet = this.registry.getStatement(keyword);
    if (stmtParselet) {
      this.advance(); // consume the keyword
      return stmtParselet(this);
    }

    // Rust `use path::to::module`, Ruby `include Module`
    if (this.peek().type === TokenType.Identifier &&
        (keyword === "use" || keyword === "include") &&
        this.peekAt(1) && (this.peekAt(1)!.type === TokenType.Identifier ||
                           this.peekAt(1)!.type === TokenType.StringLiteral)) {
      this.advance();
      return Imports.parseImport(this) as any;
    }

    // Rust `mod name;` module declaration
    if (this.peek().type === TokenType.Identifier && keyword === "mod" &&
        this.peekAt(1)?.type === TokenType.Identifier) {
      this.advance();
      const modName = this.parseIdentifier();
      this.consumeSemicolon();
      return { kind: "Import", path: modName.name, span: modName.span } as any;
    }

    // Swift operator declarations: infix/prefix/postfix operator ** { ... }
    if ((keyword === "infix" || keyword === "prefix" || keyword === "postfix") &&
        this.peekNext()?.value === "operator") {
      this.advance(); // consume infix/prefix/postfix
      this.advance(); // consume operator
      const opToken = this.advance(); // consume operator symbol
      const name: AST.Identifier = {
        kind: "Identifier",
        name: `${keyword} operator ${opToken.value}`,
        span: this.createSpanFrom(opToken)
      };
      let body: AST.Block | undefined;
      if (this.check("{")) body = this.parseBlock();
      return { kind: "FuncDecl", name, params: [], body, span: this.createSpanFrom(opToken) } as any;
    }

    const rubyDoBlock = this.tryParseRubyDoBlockExprStmt();
    if (rubyDoBlock) {
      return rubyDoBlock;
    }

    const rubyStabbyLambda = this.tryParseRubyStabbyLambdaExprStmt();
    if (rubyStabbyLambda) {
      return rubyStabbyLambda;
    }

    const rubyLabelCommand = this.tryParseRubyLabelCommandExprStmt();
    if (rubyLabelCommand) {
      return rubyLabelCommand;
    }

    // Short declarations (Go-style :=) and Python type-annotated assignments (name: Type = value)
    if (this.peek().type === TokenType.Identifier) {
      const checkpoint = this.current;
      this.advance();
      if (this.check(":=")) {
        this.current = checkpoint;
        return this.parseShortDecl() as any;
      }
      // Python type-annotated assignment: name: Type = value
      if (this.check(":") && !this.check("::")) {
        const colonCheckpoint = this.current;
        this.advance(); // consume :
        try {
          const type = this.parseType();
          if (this.check("=")) {
            this.advance(); // consume =
            const value = this.parseExpression();
            this.consumeSemicolon();
            const nameToken = this.tokens[checkpoint];
            const name: AST.Identifier = {
              kind: "Identifier",
              name: nameToken.value,
              span: this.createSpanFrom(nameToken)
            };
            return {
              kind: "VarDecl",
              names: [name],
              values: [value],
              declType: type,
              span: this.createSpan(checkpoint, this.current - 1)
            } as any;
          }
        } catch {}
        this.current = colonCheckpoint;
      }
      this.current = checkpoint;
    }

    // Block statements vs object destructuring
    if (this.check("{")) {
      const checkpoint = this.current;
      try {
        this.advance();
        let isDestructuring = false;
        if (this.peek().type === TokenType.Identifier) {
          this.advance();
          if (this.check(",") || this.check("}")) {
            isDestructuring = true;
          }
        }
        this.current = checkpoint;
        if (isDestructuring) {
          return this.parseExprStmt();
        } else {
          return Blocks.parseBlock(this);
        }
      } catch {
        this.current = checkpoint;
        return Blocks.parseBlock(this);
      }
    }

    return this.parseExprStmt();
  }

  private tryParseRubyDoBlockExprStmt(): AST.ExprStmt | null {
    const endIndex = this.findRubyDoBlockEndIndex();
    if (endIndex === undefined) return null;

    const start = this.current;
    const span = this.createSpan(start, endIndex);
    this.current = endIndex + 1;
    this.consumeSemicolon();

    return this.rawSpanExprStmt(span);
  }

  private tryParseRubyLabelCommandExprStmt(): AST.ExprStmt | null {
    const endIndex = this.findCurrentStatementEndIndex();
    if (endIndex === undefined || endIndex <= this.current) return null;
    if (!this.statementHasRubyCommandLabel(endIndex)) return null;

    const span = this.createSpan(this.current, endIndex);
    this.current = endIndex + 1;
    this.consumeSemicolon();
    return this.rawSpanExprStmt(span);
  }

  private tryParseRubyStabbyLambdaExprStmt(): AST.ExprStmt | null {
    const endIndex = this.findCurrentStatementEndIndex();
    if (endIndex === undefined || endIndex <= this.current) return null;
    if (!this.statementHasRubyStabbyLambda(endIndex)) return null;

    const start = this.current;
    const assignToken = this.tokens[start + 1];
    if (this.tokens[start]?.type === TokenType.Identifier && assignToken?.value === "=") {
      const leftToken = this.tokens[start];
      const rightSpan = this.createSpan(start + 2, endIndex);
      const right: AST.StringLiteral = {
        kind: "StringLiteral",
        parts: [{ kind: "Text", value: "" }],
        flags: {},
        delimiter: "\"",
        span: rightSpan,
      };
      const left: AST.Identifier = {
        kind: "Identifier",
        name: leftToken.value,
        span: this.createSpanFrom(leftToken),
      };
      const assign: AST.Assign = {
        kind: "Assign",
        op: "=",
        left,
        right,
        span: this.createSpan(start, endIndex),
      };
      this.current = endIndex + 1;
      this.consumeSemicolon();
      return {
        kind: "ExprStmt",
        expr: assign,
        span: assign.span,
      };
    }

    const span = this.createSpan(start, endIndex);
    this.current = endIndex + 1;
    this.consumeSemicolon();
    return this.rawSpanExprStmt(span);
  }

  private rawSpanExprStmt(span: AST.Span): AST.ExprStmt {
    const expr: AST.StringLiteral = {
      kind: "StringLiteral",
      parts: [{ kind: "Text", value: "" }],
      flags: {},
      delimiter: "\"",
      span,
    };
    return {
      kind: "ExprStmt",
      expr,
      span,
    };
  }

  private findCurrentStatementEndIndex(): number | undefined {
    let parenDepth = 0;
    let bracketDepth = 0;
    let braceDepth = 0;
    let last = this.current;

    for (let i = this.current; i < this.tokens.length; i++) {
      const token = this.tokens[i];
      if (!token || token.type === TokenType.EOF) return last;
      if ((token.virtualSemi || token.value === ";") && parenDepth === 0 && bracketDepth === 0 && braceDepth === 0) {
        return last;
      }

      if (token.value === "(") parenDepth++;
      else if (token.value === ")" && parenDepth > 0) parenDepth--;
      else if (token.value === "[") bracketDepth++;
      else if (token.value === "]" && bracketDepth > 0) bracketDepth--;
      else if (token.value === "{") braceDepth++;
      else if (token.value === "}" && braceDepth > 0) braceDepth--;

      last = i;
    }

    return last;
  }

  private statementHasRubyCommandLabel(endIndex: number): boolean {
    const first = this.peek();
    if (first.type !== TokenType.Identifier && first.type !== TokenType.Keyword) return false;

    let parenDepth = 0;
    let bracketDepth = 0;
    let braceDepth = 0;
    let sawQuestion = false;
    let sawAssignment = false;

    for (let i = this.current; i <= endIndex; i++) {
      const token = this.tokens[i];
      if (!token) break;

      if (token.value === "?") sawQuestion = true;
      if (this.isAssignmentOp(token)) sawAssignment = true;

      if (token.value === "(") parenDepth++;
      else if (token.value === ")" && parenDepth > 0) parenDepth--;
      else if (token.value === "[") bracketDepth++;
      else if (token.value === "]" && bracketDepth > 0) bracketDepth--;
      else if (token.value === "{") braceDepth++;
      else if (token.value === "}" && braceDepth > 0) braceDepth--;

      const next = this.tokens[i + 1];
      const isLabel = (token.type === TokenType.Identifier || token.type === TokenType.Keyword) &&
        next?.value === ":" &&
        token.end === next.start &&
        this.tokens[i + 2]?.value !== ":" &&
        parenDepth === 0 &&
        bracketDepth === 0 &&
        braceDepth === 0;
      const isSpacedSymbolArg = token.value === ":" &&
        next &&
        (next.type === TokenType.Identifier || next.type === TokenType.Keyword) &&
        i > this.current &&
        this.tokens[i - 1]?.end < token.start &&
        parenDepth === 0 &&
        bracketDepth === 0 &&
        braceDepth === 0;

      if (isLabel || isSpacedSymbolArg) {
        return i > this.current && !sawQuestion && !sawAssignment;
      }
    }

    return false;
  }

  private statementHasRubyStabbyLambda(endIndex: number): boolean {
    for (let i = this.current; i <= endIndex; i++) {
      if (this.tokens[i]?.value === "->") {
        return true;
      }
    }
    return false;
  }

  private findRubyDoBlockEndIndex(): number | undefined {
    let parenDepth = 0;
    let bracketDepth = 0;
    let braceDepth = 0;
    let doIndex: number | undefined;

    for (let i = this.current; i < this.tokens.length; i++) {
      const token = this.tokens[i];
      if (!token || token.type === TokenType.EOF || token.virtualSemi || token.value === ";") break;

      if (token.value === "(") parenDepth++;
      else if (token.value === ")" && parenDepth > 0) parenDepth--;
      else if (token.value === "[") bracketDepth++;
      else if (token.value === "]" && bracketDepth > 0) bracketDepth--;
      else if (token.value === "{") braceDepth++;
      else if (token.value === "}" && braceDepth > 0) braceDepth--;

      if (token.value === "do" && parenDepth === 0 && bracketDepth === 0 && braceDepth === 0) {
        doIndex = i;
        break;
      }
    }

    if (doIndex === undefined) return undefined;

    let rubyDepth = 1;
    for (let i = doIndex + 1; i < this.tokens.length; i++) {
      const token = this.tokens[i];
      if (!token || token.type === TokenType.EOF) return undefined;
      const value = token.value;

      if (value === "do" || value === "class" || value === "module" || value === "def" ||
          value === "begin" || value === "if" || value === "unless" || value === "case" ||
          value === "while" || value === "until" || value === "for") {
        rubyDepth++;
      } else if (value === "end") {
        rubyDepth--;
        if (rubyDepth === 0) return i;
      }
    }

    return undefined;
  }
  
  
  // Block parsing — delegated to src/parselets/blocks.ts (Chunk 9)
  public parseBlock(): AST.Block { return Blocks.parseBlock(this); }
  public parseBlockOrStatement(): AST.Block { return Blocks.parseBlockOrStatement(this); }
  public parseIndentBlock(): AST.Block { return Blocks.parseIndentBlock(this); }
  public parseKeywordBlock(keyword?: string): AST.Block { return Blocks.parseKeywordBlock(this, keyword); }
  public parseBashTestExpression(): AST.Expr { return Blocks.parseBashTestExpression(this); }
  public parseIfThenBlock(): AST.Block { return Blocks.parseIfThenBlock(this); }
  public checkIndentBlock(): boolean { return Blocks.checkIndentBlock(this); }
  public parseSwitch(): AST.Switch | AST.Match { return Blocks.parseSwitch(this); }

  // Expression prefix/postfix — delegated to src/parselets/expr-*.ts (Chunk 12)
  public parsePrimary(): AST.Expr { return ExprPrefix.parsePrimary(this); }
  public parsePostfix(expr: AST.Expr): AST.Expr { return ExprPostfix.parsePostfix(this, expr); }
  public parseArguments(): AST.Expr[] { return ExprPostfix.parseArguments(this); }
  public tryParseGenericArgs(): AST.TypeNode[] | null { return ExprPostfix.tryParseGenericArgs(this); }
  public looksLikeFuncCall(): boolean { return ExprPrefix.looksLikeFuncCall(this); }
  public shouldReinterpretAsIdentifier(): boolean { return ExprPrefix.shouldReinterpretAsIdentifier(this); }
  public parseBacktickIdentifier(): AST.Identifier { return ExprPrefix.parseBacktickIdentifier(this); }
  public parseNewExpression(): AST.Expr { return ExprPrefix.parseNewExpression(this); }
  public parseGoCompositeLiteral(): AST.Expr { return ExprPrefix.parseGoCompositeLiteral(this); }

  // Expression parsing with Pratt parser
  public parseExpression(minPrecedence = 0): AST.Expr {
    let left = this.parsePrimary();
    
    // Check for single-parameter lambda without parentheses
    if (left.kind === "Identifier" && this.check("=>")) {
      this.advance(); // consume =>
      
      // Skip virtual semicolons after => in arrow functions
      while (this.peek().virtualSemi) {
        this.advance();
      }
      
      const body = this.check("{") ? Blocks.parseBlock(this) : this.parseAssignmentExpression();
      return {
        kind: "Lambda",
        params: [{
          name: left as AST.Identifier,
          type: undefined,
          defaultValue: undefined,
          span: left.span
        }],
        returnType: undefined,
        body,
        span: this.createSpanFrom(left)
      };
    }
    
    while (true) {
      // Skip JSX whitespace tokens if we're in a JSX context
      JSX.skipJSXWhitespace(this);
      
      const op = this.peek();
      
      // Check for 'as' type assertion (TypeScript)
      if (op.value === "as") {
        this.advance(); // consume 'as'
        while (this.peek().virtualSemi) this.advance(); // skip vsemi before type
        const type = this.parseType();
        left = {
          kind: "TypeAssertion",
          expr: left,
          type,
          span: this.createSpanFrom(left)
        };
        continue;
      }
      
      // Python "not in" compound operator
      if (op.value === "not" && this.peekNext()?.value === "in") {
        this.advance(); // consume 'not'
        this.advance(); // consume 'in'
        while (this.peek().virtualSemi) this.advance();
        const right = this.parseExpression(12); // same precedence as 'in'
        left = {
          kind: "Binary",
          op: "not in",
          left,
          right,
          span: this.createSpanFrom(left)
        };
        continue;
      }

      // Python "is not" compound operator
      if (op.value === "is" && this.peekNext()?.value === "not") {
        this.advance(); // consume 'is'
        this.advance(); // consume 'not'
        while (this.peek().virtualSemi) this.advance();
        const right = this.parseExpression(12);
        left = {
          kind: "Binary",
          op: "is not",
          left,
          right,
          span: this.createSpanFrom(left)
        };
        continue;
      }

      // Python ternary: value if condition else alternative
      // Only if we can find 'else' before statement-ending tokens
      if (op.value === "if" && op.type === TokenType.Keyword) {
        let pos = this.current + 1; // past 'if'
        let depth = 0;
        let foundElse = false;
        while (pos < this.tokens.length) {
          const v = this.tokens[pos].value;
          // Break on block-starting { at depth 0 — not a Python ternary
          if (depth === 0 && v === "{") break;
          if (v === "(" || v === "[" || v === "{") depth++;
          else if (v === ")" || v === "]" || v === "}") {
            if (depth === 0) break;
            depth--;
          }
          else if (depth === 0 && v === "else") { foundElse = true; break; }
          else if (depth === 0 && (v === "for" || v === "," || v === ";" || this.tokens[pos].virtualSemi)) break;
          pos++;
        }
        if (foundElse) {
          this.advance(); // consume 'if'
          while (this.peek().virtualSemi) this.advance();
          const condition = this.parseExpression(2);
          this.consume("else", "Expected 'else' in Python ternary");
          while (this.peek().virtualSemi) this.advance();
          const alternate = this.parseExpression(2);
          left = {
            kind: "Ternary",
            test: condition,
            consequent: left,
            alternate,
            span: this.createSpanFrom(left)
          };
          continue;
        }
      }

      if (!this.isBinaryOp(op) && !this.isAssignmentOp(op)) {
        break;
      }

      const precedence = this.getPrecedence(op);
      if (precedence < minPrecedence) {
        break;
      }
      
      this.advance();
      
      // Skip JSX whitespace after operators
      JSX.skipJSXWhitespace(this);
      
      // Skip virtual semicolons after binary operators
      while (this.peek().virtualSemi) {
        this.advance();
      }

      // Trailing comma: if comma was consumed but next token closes a group, un-consume
      if (op.value === "," && (this.check(")") || this.check("]") || this.check("}"))) {
        break;
      }

      // Handle ternary operator
      if (op.value === "?") {
        JSX.skipJSXWhitespace(this);
        const consequent = this.parseExpression();
        JSX.skipJSXWhitespace(this);
        while (this.peek().virtualSemi) this.advance();
        this.consume(":", "Expected ':' in ternary expression");
        JSX.skipJSXWhitespace(this);
        const alternate = this.parseExpression(precedence);
        left = {
          kind: "Ternary",
          test: left,
          consequent,
          alternate,
          span: this.createSpanFrom(left)
        };
        continue;
      }
      
      const isRightAssoc = this.isRightAssociative(op);
      const nextMinPrec = isRightAssoc ? precedence : precedence + 1;
      
      // Special case: pipe operator followed by match expression
      let right: AST.Expr;
      if (op.value === "|>" && this.check("match")) {
        // Parse match with implicit discriminant (left side of pipe)
        this.advance(); // consume 'match'
        const switchStmt = Blocks.parseSwitch(this);
        // For now, treat it as an expression (would need AST changes for proper support)
        right = switchStmt as any;
      } else {
        right = this.parseExpression(nextMinPrec);
      }
      
      if (this.isAssignmentOp(op)) {
        left = {
          kind: "Assign",
          op: op.value,
          left,
          right,
          span: this.createSpanFrom(left)
        };
      } else {
        left = {
          kind: "Binary",
          op: op.value,
          left,
          right,
          span: this.createSpanFrom(left)
        };
      }
    }
    
    return left;
  }
  
  
  
  // Helper methods for specific constructs
  // Variable/const/short declarations — delegated to src/parselets/declarations.ts
  public parseVarDecl(): AST.VarDecl { return Decls.parseVarDecl(this); }
  public parseConstDecl(): AST.ConstDecl { return Decls.parseConstDecl(this); }
  private parseShortDecl(): AST.ShortDecl { return Decls.parseShortDecl(this); }
  private parseDestructuringShortDecl(): AST.ShortDecl { return Decls.parseDestructuringShortDecl(this); }
  public parseDestructuringPattern(): AST.ArrayPattern | AST.ObjectPattern { return Decls.parseDestructuringPattern(this); }
  private peekAhead(value: string): boolean { return Decls.peekAhead(this, value); }
  
  public parseExpressionBody(): AST.Block {
    const expr = this.parseExpression();
    return {
      kind: "Block",
      statements: [{
        kind: "Return",
        values: [expr],
        span: expr.span
      }],
      span: expr.span
    };
  }
  
  // Control flow parsing
  
  
  
  public parseExprStmt(): AST.ExprStmt | AST.If {
    const expr = this.parseExpression();
    
    // Check for reassignment operator :=:
    if (this.match(":=:")) {
      if (expr.kind !== "Identifier") {
        throw this.error(this.previous()!, "Reassignment requires an identifier");
      }
      const value = this.parseExpression();
      this.consumeSemicolon();
      return {
        kind: "ExprStmt",
        expr: {
          kind: "Assign",
          op: ":=:",
          left: expr,
          right: value,
          span: this.createSpanFrom(expr)
        },
        span: this.createSpanFrom(expr)
      };
    }
    
    // Check for postfix if/unless (Ruby-style) — only on same line
    if ((this.check("if") || this.check("unless")) && !this.peek().newline) {
      const modifier = this.peek().value;
      this.advance();
      const condition = this.parseExpression();
      
      // Create an If statement that executes expr if condition is true (or false for unless)
      const ifStmt: AST.If = {
        kind: "If",
        arms: [{
          test: modifier === "unless" ? {
            kind: "Unary",
            op: "!",
            argument: condition,
            prefix: true,
            span: condition.span
          } : condition,
          body: {
            kind: "Block",
            statements: [{
              kind: "ExprStmt",
              expr,
              span: expr.span
            }],
            span: expr.span
          },
          span: this.createSpanFrom(expr)
        }],
        span: this.createSpanFrom(expr)
      };
      
      this.consumeSemicolon();
      
      return ifStmt;
    }
    
    this.consumeSemicolon();
    
    return {
      kind: "ExprStmt",
      expr,
      span: expr.span
    };
  }
  
  // Type parsing
  public parseType(): AST.TypeNode {
    return Types.parseType(this);
  }
  
  
  
  // Type/package/export declarations — delegated to src/parselets/declarations.ts
  private parseTypeDecl(): AST.TypeDecl { return Decls.parseTypeDecl(this); }
  private parsePackageDecl(): AST.PackageDecl { return Decls.parsePackageDecl(this); }
  private parseExportDecl(): AST.ExportDecl { return Decls.parseExportDecl(this); }

  // Literal parsing
  // Literals extracted to src/parselets/literals.ts (Chunk 6)
  public parseStringLiteral(): AST.StringLiteral {
    return Literals.parseStringLiteral(this);
  }

  
  // Helper methods
  
  public parseIdentifier(): AST.Identifier {
    const token = this.peek();
    
    // Allow keywords as identifiers in member access context
    if (token.type === TokenType.Keyword && this.shouldReinterpretAsIdentifier()) {
      this.advance();
      return {
        kind: "Identifier",
        name: token.value,
        originalSpelling: token.value,
        span: this.createSpanFrom(token)
      };
    }
    
    if (token.type === TokenType.Identifier || 
        token.type === TokenType.SigilIdentifier) {
      this.advance();
      
      // Handle sigil identifiers
      const name = token.type === TokenType.SigilIdentifier ?
        token.value.slice(1) : // Remove $ prefix
        token.value;
      
      return {
        kind: "Identifier",
        name,
        originalSpelling: token.value,
        span: this.createSpanFrom(token)
      };
    }
    
    // Check for backtick identifier
    if (token.type === TokenType.TemplateLiteral && 
        this.shouldReinterpretAsIdentifier()) {
      return this.parseBacktickIdentifier();
    }
    
    // Return missing identifier instead of throwing
    this.errors.push(new ParseError("Expected identifier", token));
    return this.createMissingIdentifier();
  }
  
  public parseExpressionList(): AST.Expr[] {
    const exprs: AST.Expr[] = [this.parseAssignmentExpression()];
    
    while (this.match(",")) {
      exprs.push(this.parseAssignmentExpression());
    }
    
    return exprs;
  }
  
  
  public parseAssignmentExpression(): AST.Expr {
    // Parse everything except comma operator
    return this.parseExpression(this.getPrecedence({value: ","} as Token) + 1);
  }
  
  
  // Operator precedence
  public getPrecedence(token: Token): number {
    switch (token.value) {
      // Highest precedence (handled in parsePostfix)
      // case "(": case "[": case ".": case "?.": return 18;
      
      // Unary operators (handled in parsePrimary)
      // case "new": case "++": case "--": return 17;
      // case "!": case "~": case "+": case "-": case "typeof": case "void": case "delete": case "await": return 16;
      
      case "**": return 15;
      case "*": case "/": case "%": return 14;
      case "+": case "-": return 13;
      case "<<": case ">>": case ">>>": return 12;
      case "..": return 11.5; // Range operator
      case "<-": return 11.3; // Channel send operator
      case "<": case "<=": case ">": case ">=": case "in": case "instanceof": case "is": return 11;
      case "<=>": return 11; // Spaceship operator (comparison)
      case "==": case "!=": case "===": case "!==": return 10;
      case "=~": return 10;
      case "|>": return 9; // Pipeline operator
      case "&": return 9;
      case "^": return 8;
      case "|": return 7;
      case "&&": case "and": return 6;
      case "||": case "or": return 5;
      case "??": return 4;
      case "?": return 3; // Ternary
      case "=": case "+=": case "-=": case "*=": case "/=": case "%=":
      case "**=": case "<<=": case ">>=": case ">>>=":
      case "&=": case "^=": case "|=": case "??=":
      case ":=": case ":=:": return 2;
      case ",": return 1;
      
      default: return 0;
    }
  }
  
  /**
   * Check if `func` or `function` looks like a function call rather than a function expression.
   * func() with no body following the parens = function call.
   * func() { ... } or func name() { ... } = function expression.
   */

  public isRightAssociative(token: Token): boolean {
    return token.value === "**" || this.isAssignmentOp(token);
  }
  
  public isBinaryOp(token: Token): boolean {
    return this.getPrecedence(token) > 0 && !this.isAssignmentOp(token);
  }
  
  public isAssignmentOp(token: Token): boolean {
    const op = token.value;
    return op === "=" || op === "+=" || op === "-=" || op === "*=" ||
           op === "/=" || op === "%=" || op === "**=" || op === "<<=" ||
           op === ">>=" || op === ">>>=" || op === "&=" || op === "^=" ||
           op === "|=" || op === "??=" || op === "||=" || op === "&&=" ||
           op === ":=" || op === ":=:";
  }
  
  public isUnaryOp(token: Token): boolean {
    const op = token.value;
    return op === "!" || op === "~" || op === "+" || op === "-" ||
           op === "typeof" || op === "void" || op === "delete" || op === "not" ||
           op === "await" || op === "++" || op === "--" || op === "&" || op === "*" || op === "**" ||
           op === "->";
  }

  // Override synchronize to include isDeclStart() check
  public override synchronize(): void {
    this.advance();

    while (!this.isAtEnd()) {
      if (this.previous()?.type === TokenType.Operator &&
          this.previous()?.value === ";") {
        return;
      }

      const token = this.peek();
      if (token.value === "fi" || token.value === "esac" || token.value === "done" ||
          token.value === "end" || token.value === "}" || token.value === "elif" ||
          token.value === "else" || token.value === "elseif" || token.value === "rescue" ||
          token.value === "ensure" || token.value === "except" || token.value === "finally") {
        return;
      }

      if (this.isDeclStart()) {
        return;
      }

      this.advance();
    }
  }

  // JSX methods extracted to src/parselets/jsx.ts (Chunk 2)
}
