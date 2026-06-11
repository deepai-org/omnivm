import { applyMASI } from './lexer-masi';
import * as LS from './lex-state';
import { LexerCursor } from './lexer-cursor';
import { skipShebang, skipLineComment, skipBlockComment, skipHTMLComment, trySkipMultilineRustAttribute } from './lexer-comments';
import { scanPrefixedString, scanTemplateLiteral, scanNumber, scanHeredoc, scanRegex } from './lexer-literals';
import { scanIdentifier, scanSigilIdentifier } from './lexer-identifiers';
import { scanOperator, shouldBeRegex } from './lexer-operators';

export enum TokenType {
  // Literals
  NumericLiteral = "NumericLiteral",
  StringLiteral = "StringLiteral",
  TemplateLiteral = "TemplateLiteral",
  RegexLiteral = "RegexLiteral",
  
  // Identifiers
  Identifier = "Identifier",
  SigilIdentifier = "SigilIdentifier",
  Keyword = "Keyword",
  
  // Operators and Punctuators
  Operator = "Operator",
  
  // JSX Tokens
  JSXTagStart = "JSXTagStart",      // < when starting JSX
  
  // Structure
  Comment = "Comment",
  Whitespace = "Whitespace",
  VirtualSemi = "VirtualSemi",
  EOF = "EOF"
}

export interface Token {
  type: TokenType;
  value: string;
  start: number;
  end: number;
  line: number;
  column: number;
  virtualSemi?: boolean;
  indentCol?: number;
  newline?: boolean;
}

export class Lexer extends LexerCursor {
  // HTML tag names for JSX detection per spec
  readonly htmlTags = new Set([
    'div', 'span', 'p', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'ul', 'ol', 'li',
    'a', 'img', 'input', 'button', 'form', 'section', 'article', 'header', 'footer',
    'nav', 'main', 'aside', 'table', 'tr', 'td', 'th', 'tbody', 'thead', 'tfoot',
    'select', 'option', 'textarea', 'label', 'fieldset', 'legend', 'canvas', 'video',
    'audio', 'source', 'track', 'embed', 'object', 'iframe', 'script', 'style',
    'meta', 'link', 'title', 'head', 'body', 'html', 'strong', 'em', 'b', 'i',
    'small', 'mark', 'del', 'ins', 'sub', 'sup', 'code', 'pre', 'kbd', 'samp',
    'var', 'time', 'data', 'address', 'cite', 'q', 'abbr', 'dfn', 'ruby', 'rt',
    'rp', 'bdi', 'bdo', 'wbr', 'details', 'summary', 'dialog', 'menu'
  ]);

  constructor(source: string) {
    super(source);
  }
  
  tokenize(): Token[] {
    // Skip shebang if present. `#![...]` is a Rust inner attribute, not a
    // shebang (real shebangs are `#!/usr/bin/...`).
    if (this.source.startsWith('#!') && !this.source.startsWith('#![')) {
      skipShebang(this);
    }

    while (this.position < this.source.length) {
      this.scanToken();
    }

    // Add EOF token
    this.addToken(TokenType.EOF, '', this.position, this.position);

    // Apply MASI (Max-Accept Semicolon Insertion)
    this.tokens = applyMASI(this.tokens);

    return this.tokens;
  }

  private scanToken(): void {
    const start = this.position;
    const startLine = this.line;
    const startColumn = this.column;
    
    const char = this.advance();

    // Line continuation: backslash followed by newline — skip both
    if (char === '\\' && this.peek() === '\n') {
      this.advance(); // consume newline
      this.line++;
      this.column = 0;
      return;
    }

    // Comments
    if (char === '/' && this.peek() === '/') {
      skipLineComment(this);
      return;
    }

    if (char === '/' && this.peek() === '*') {
      skipBlockComment(this);
      return;
    }

    // Hash comments (Python/Ruby style). Rust attributes (`#[...]` /
    // `#![...]`) whose balanced bracket group spans multiple lines are
    // consumed wholly so the continuation lines never leak into the token
    // stream; single-line attributes are ordinary hash comments already.
    if (char === '#') {
      if (!trySkipMultilineRustAttribute(this)) {
        skipLineComment(this);
      }
      return;
    }

    // Double-dash comments
    if (char === '-' && this.peek() === '-' &&
        (this.position === 1 || /\s/.test(this.source[this.position - 2]))) {
      this.advance(); // consume second -
      skipLineComment(this);
      return;
    }

    if (char === '<' && this.peek() === '!' && this.peekNext() === '-' && this.peekAt(2) === '-') {
      skipHTMLComment(this);
      return;
    }
    
    // Whitespace
    if (/\s/.test(char)) {
      // In JSX text, preserve whitespace as text tokens
      if (LS.shouldPreserveWhitespace(this.state)) {
        let whitespaceValue = char;
        const start = this.position - 1;
        const startLine = this.line;
        const startColumn = this.column - 1;
        
        // Collect consecutive whitespace
        while (/\s/.test(this.peek()) && !this.isAtEnd()) {
          const wsChar = this.advance();
          whitespaceValue += wsChar;
          if (wsChar === '\n') {
            this.line++;
            this.column = 0;
            this.lineStart = true;
          }
        }
        
        this.addTokenEx(TokenType.StringLiteral, whitespaceValue, start, this.position, startLine, startColumn);
        return;
      }
      
      // Normal whitespace handling - track line starts and indentation
      if (char === '\n') {
        this.lineStart = true;
        this.line++;
        this.column = 0;
        
        // Count spaces at start of next line for indentation
        let indent = 0;
        let pos = this.position;
        while (pos < this.source.length && this.source[pos] === ' ') {
          indent++;
          pos++;
        }
        this.currentIndent = indent;
      }
      
      // Skip remaining whitespace
      while (/\s/.test(this.peek()) && !this.isAtEnd()) {
        if (this.peek() === '\n') {
          this.line++;
          this.column = 0;
          this.lineStart = true;
          
          // Update indent for next line
          this.advance();
          let indent = 0;
          let pos = this.position;
          while (pos < this.source.length && this.source[pos] === ' ') {
            indent++;
            pos++;
          }
          this.currentIndent = indent;
        } else {
          this.advance();
        }
      }
      return;
    }
    
    // String literals with prefixes
    if (/[rbfuRBFU]/.test(char)) {
      const next = this.peek();
      if (next === '"' || next === "'") {
        this.position--; // Back up to include prefix
        scanPrefixedString(this);
        return;
      }
    }

    // C# verbatim strings (@"") or C# interpolated strings ($"")
    if ((char === '@' || char === '$') && this.peek() === '"') {
      this.position--; // Back up
      scanPrefixedString(this);
      return;
    }

    // String literals
    if (char === '"' || char === "'") {
      this.position--; // Back up
      scanPrefixedString(this);
      return;
    }

    // Template literals
    if (char === '`') {
      scanTemplateLiteral(this);
      return;
    }

    // Numbers
    if (/\d/.test(char)) {
      scanNumber(this);
      return;
    }

    // Identifiers and keywords
    if (/[a-zA-Z_]/.test(char)) {
      scanIdentifier(this);
      return;
    }
    
    // Sigil identifiers and bash special variables
    if (char === '$') {
      const next = this.peek();
      if (/[a-zA-Z_]/.test(next)) {
        scanSigilIdentifier(this);
        return;
      }
      // Bash special variables: $?, $!, $#, $@, $$, $0-$9
      if (next === '?' || next === '!' || next === '#' || next === '@' || next === '$' || /[0-9]/.test(next)) {
        const start = this.position - 1;
        const startLine = this.line;
        const startColumn = this.column - 1;
        const value = '$' + this.advance();
        this.addTokenEx(TokenType.SigilIdentifier, value, start, this.position, startLine, startColumn);
        return;
      }
      // Bash command substitution $(...) — scan as sigil identifier
      if (next === '(') {
        const start = this.position - 1;
        const startLine = this.line;
        const startColumn = this.column - 1;
        this.advance(); // consume '('
        let depth = 1;
        let value = '$(';
        while (depth > 0 && !this.isAtEnd()) {
          const c = this.advance();
          if (c === '(') depth++;
          else if (c === ')') depth--;
          if (depth > 0) value += c;
        }
        value += ')';
        this.addTokenEx(TokenType.SigilIdentifier, value, start, this.position, startLine, startColumn);
        return;
      }
      // Bash ${...} variable expansion
      if (next === '{') {
        const start = this.position - 1;
        const startLine = this.line;
        const startColumn = this.column - 1;
        this.advance(); // consume '{'
        let value = '${';
        while (this.peek() !== '}' && !this.isAtEnd()) {
          value += this.advance();
        }
        if (this.peek() === '}') {
          value += this.advance();
        }
        this.addTokenEx(TokenType.SigilIdentifier, value, start, this.position, startLine, startColumn);
        return;
      }
      // Fall through — emit $ as an operator
    }
    
    // Heredoc check (before operators)
    if (char === '<' && this.peek() === '<') {
      const nextChar = this.peekNext();
      // Only treat as heredoc if followed by uppercase letter (common convention)
      if (nextChar && /[A-Z]/.test(nextChar)) {
        this.advance(); // consume second '<'
        if (scanHeredoc(this)) return;
        // No delimiter found — fall through to operator scan
        scanOperator(this, this.htmlTags);
        return;
      }
    }
    
    // Regex or division
    if (char === '/') {
      if (shouldBeRegex(this)) {
        scanRegex(this);
      } else {
        scanOperator(this, this.htmlTags);
      }
      return;
    }

    // Operators and punctuators
    scanOperator(this, this.htmlTags);
  }
  
  private isHTMLTag(tagName: string): boolean {
    return this.htmlTags.has(tagName.toLowerCase());
  }
}