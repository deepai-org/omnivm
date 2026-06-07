/**
 * Dialect Context — Capability masks for language-specific parsing (Chunk 11).
 *
 * Replaces ad-hoc boolean checks with a structured capability system.
 * Not a grand theory — just a capability mask replacing scattered conditionals.
 */

export interface DialectCapabilities {
  /** Python-style indent-delimited blocks */
  supportsIndentBlocks: boolean;
  /** Ruby/Bash keyword-delimited blocks (do...end, begin...end) */
  supportsKeywordBlocks: boolean;
  /** Ruby do...end blocks attached to method calls */
  supportsRubyBlocks: boolean;
  /** JSX element syntax (<Component />) */
  supportsJSX: boolean;
  /** TypeScript-style type assertions (<Type>expr) */
  supportsTypeAssertions: boolean;
  /** Bash case...esac */
  supportsCaseEsac: boolean;
  /** Go for-range loops */
  supportsGoForRange: boolean;
  /** Rust-style impl blocks */
  supportsImplBlocks: boolean;
  /** Pattern matching (match/when expressions) */
  supportsPatternMatching: boolean;
  /** Go-style short variable declarations (:=) */
  supportsShortDecl: boolean;
}

/**
 * Default capabilities — PolyScript's "parse everything" mode.
 * All capabilities are enabled since PolyScript accepts any syntax.
 */
export const POLYGLOT_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: true,
  supportsKeywordBlocks: true,
  supportsRubyBlocks: true,
  supportsJSX: true,
  supportsTypeAssertions: true,
  supportsCaseEsac: true,
  supportsGoForRange: true,
  supportsImplBlocks: true,
  supportsPatternMatching: true,
  supportsShortDecl: true,
};

/** JavaScript/TypeScript dialect */
export const JS_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: false,
  supportsKeywordBlocks: false,
  supportsRubyBlocks: false,
  supportsJSX: true,
  supportsTypeAssertions: true,
  supportsCaseEsac: false,
  supportsGoForRange: false,
  supportsImplBlocks: false,
  supportsPatternMatching: false,
  supportsShortDecl: false,
};

/** Python dialect */
export const PYTHON_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: true,
  supportsKeywordBlocks: false,
  supportsRubyBlocks: false,
  supportsJSX: false,
  supportsTypeAssertions: false,
  supportsCaseEsac: false,
  supportsGoForRange: false,
  supportsImplBlocks: false,
  supportsPatternMatching: true,
  supportsShortDecl: false,
};

/** Go dialect */
export const GO_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: false,
  supportsKeywordBlocks: false,
  supportsRubyBlocks: false,
  supportsJSX: false,
  supportsTypeAssertions: false,
  supportsCaseEsac: false,
  supportsGoForRange: true,
  supportsImplBlocks: false,
  supportsPatternMatching: false,
  supportsShortDecl: true,
};

/** Rust dialect */
export const RUST_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: false,
  supportsKeywordBlocks: false,
  supportsRubyBlocks: false,
  supportsJSX: false,
  supportsTypeAssertions: false,
  supportsCaseEsac: false,
  supportsGoForRange: false,
  supportsImplBlocks: true,
  supportsPatternMatching: true,
  supportsShortDecl: false,
};

/** Ruby dialect */
export const RUBY_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: false,
  supportsKeywordBlocks: true,
  supportsRubyBlocks: true,
  supportsJSX: false,
  supportsTypeAssertions: false,
  supportsCaseEsac: false,
  supportsGoForRange: false,
  supportsImplBlocks: false,
  supportsPatternMatching: true,
  supportsShortDecl: false,
};

/** Bash dialect */
export const BASH_CAPABILITIES: DialectCapabilities = {
  supportsIndentBlocks: false,
  supportsKeywordBlocks: true,
  supportsRubyBlocks: false,
  supportsJSX: false,
  supportsTypeAssertions: false,
  supportsCaseEsac: true,
  supportsGoForRange: false,
  supportsImplBlocks: false,
  supportsPatternMatching: false,
  supportsShortDecl: false,
};
