import * as AST from '../ast';

/**
 * Supported OmniVM runtimes.
 */
export enum OmniRuntime {
  Python = "python",
  JavaScript = "javascript",
  Go = "go",
  Ruby = "ruby",
  Java = "java",
  Rust = "rust",
  C = "c",
}

/**
 * Evidence for why a runtime was assigned.
 */
export interface AffinityEvidence {
  type: "node_type" | "keyword" | "import" | "method" | "builtin" | "scope" | "directive" | "runtime_tag" | "syntax" | "fallback";
  detail: string;
}

/**
 * Runtime affinity assigned to an AST node.
 */
export interface RuntimeAffinity {
  runtime: OmniRuntime;
  confidence: "definite" | "inferred" | "fallback";
  evidence: AffinityEvidence[];
  async?: boolean;
}

/**
 * Describes a bridge crossing between two runtimes.
 */
export interface BridgeDescriptor {
  from: OmniRuntime;
  to: OmniRuntime;
  marshalKind: MarshalKind;
  cost: number;
  async?: boolean;
}

/**
 * Type of data being marshalled across a bridge.
 */
export enum MarshalKind {
  Primitive = "primitive",
  Array = "array",
  Object = "object",
  Callback = "callback",
  AsyncBridge = "async_bridge",
  Unknown = "unknown",
}

/**
 * Wraps an AST node with runtime affinity metadata.
 * Does not mutate the original AST.
 */
export interface AnnotatedNode<T = AST.Decl | AST.Stmt | AST.Expr> {
  node: T;
  affinity: RuntimeAffinity;
  bridge?: BridgeDescriptor;
  children: AnnotatedNode[];
}

/**
 * The full annotated program — the output of the RuntimeResolver.
 */
export interface AnnotatedProgram {
  program: AST.Program;
  root: AnnotatedNode<AST.Program>;
  affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>;
  bridges: BridgeDescriptor[];
  defaultRuntime: OmniRuntime;
  /** Original source text — enables span-based source extraction for codegen. */
  source?: string;
}

/**
 * Symbol entry tracked by the resolver's symbol table.
 */
export interface SymbolEntry {
  name: string;
  affinity: RuntimeAffinity;
  declNode?: AST.Decl | AST.Stmt | AST.Expr;
}
