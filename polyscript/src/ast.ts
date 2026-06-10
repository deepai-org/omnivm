export interface Span {
  start: number;
  end: number;
  line: number;
  column: number;
}

// Type nodes
export type TypeNode =
  | SimpleType
  | NullableType
  | UnionType
  | GenericType
  | FuncType
  | ChanType
  | ImplType
  | DynType
  | IndexedAccessType
  | ObjectType;

export interface SimpleType {
  kind: "SimpleType";
  id: Identifier;
  span: Span;
}

export interface NullableType {
  kind: "NullableType";
  inner: TypeNode;
  span: Span;
}

export interface UnionType {
  kind: "UnionType";
  types: TypeNode[];
  span: Span;
}

export interface GenericType {
  kind: "GenericType";
  base: Identifier;
  args: TypeNode[];
  span: Span;
}

export interface FuncType {
  kind: "FuncType";
  params: FuncTypeParam[];  // Changed to store names with types
  ret: TypeNode;
  span: Span;
}

export interface FuncTypeParam {
  name?: Identifier;  // Optional parameter name
  type: TypeNode;
  optional?: boolean;
  span: Span;
}

export interface ChanType {
  kind: "ChanType";
  direction: "send" | "receive" | "both";
  elementType?: TypeNode;
  span: Span;
}

export interface ImplType {
  kind: "ImplType";
  trait: TypeNode;
  span: Span;
}

export interface DynType {
  kind: "DynType";
  trait: TypeNode;
  span: Span;
}

export interface IndexedAccessType {
  kind: "IndexedAccessType";
  object: TypeNode;
  index: string;
  span: Span;
}

export interface ObjectType {
  kind: "ObjectType";
  properties: ObjectTypeProperty[];
  span: Span;
}

export interface ObjectTypeProperty {
  name: string;
  type: TypeNode;
  optional?: boolean;
  readonly?: boolean;
}

// Expression nodes
export type Expr =
  | NumericLiteral
  | StringLiteral
  | RegexLiteral
  | BooleanLiteral
  | NullLiteral
  | Identifier
  | NewExpr
  | Call
  | Index
  | Member
  | Unary
  | Binary
  | Assign
  | Lambda
  | Ternary
  | ArrayLiteral
  | SetLiteral
  | ObjectLiteral
  | ListComprehension
  | Spread
  | Yield
  | Go
  | TypeAssertion
  | JSXElement
  | JSXFragment
  | Match
  | RuntimeTag;

export interface NumericLiteral {
  kind: "NumericLiteral";
  raw: string;
  base: "decimal" | "hex" | "octal" | "binary";
  suffix?: string;
  span: Span;
}

export interface StringLiteral {
  kind: "StringLiteral";
  parts: StringPart[];
  flags: {
    raw?: boolean;
    bytes?: boolean;
    format?: boolean;
    const?: boolean;
  };
  delimiter: string;
  span: Span;
}

export interface StringPart {
  kind: "Text" | "Interpolation";
  value: string | Expr;
}

export interface RegexLiteral {
  kind: "RegexLiteral";
  pattern: string;
  flags: string;
  span: Span;
}

export interface BooleanLiteral {
  kind: "BooleanLiteral";
  value: boolean;
  span: Span;
}

export interface NullLiteral {
  kind: "NullLiteral";
  span: Span;
}

export interface Identifier {
  kind: "Identifier";
  name: string;
  originalSpelling?: string; // For $foo or `backtick` identifiers
  span: Span;
}

export interface Call {
  kind: "Call";
  callee: Expr;
  args: Expr[];
  typeArgs?: TypeNode[];
  /** Type args were spelled with Rust turbofish syntax (`::<T, U>`). */
  turbofish?: boolean;
  optional?: boolean;
  span: Span;
}

export interface NewExpr {
  kind: "NewExpr";
  callee: Expr;
  args: Expr[];
  typeArgs?: TypeNode[];
  span: Span;
}

export interface Index {
  kind: "Index";
  object: Expr;
  index: Expr;
  optional?: boolean;
  span: Span;
}

export interface Member {
  kind: "Member";
  object: Expr;
  property: Identifier;
  optional?: boolean;
  computed?: boolean;
  span: Span;
}

export interface Unary {
  kind: "Unary";
  op: string;
  argument: Expr;
  prefix: boolean;
  span: Span;
}

export interface Binary {
  kind: "Binary";
  op: string;
  left: Expr;
  right: Expr;
  span: Span;
}

export interface Assign {
  kind: "Assign";
  op: string;
  left: Expr;
  right: Expr;
  span: Span;
}

export interface Lambda {
  kind: "Lambda";
  params: Param[];
  returnType?: TypeNode;
  body: Block | Expr;
  async?: boolean;
  unsafe?: boolean;
  span: Span;
}

export interface Ternary {
  kind: "Ternary";
  test: Expr;
  consequent: Expr;
  alternate: Expr;
  span: Span;
}

export interface ArrayLiteral {
  kind: "ArrayLiteral";
  elements: Expr[];
  span: Span;
}

export interface SetLiteral {
  kind: "SetLiteral";
  elements: Expr[];
  span: Span;
}

export interface ObjectLiteral {
  kind: "ObjectLiteral";
  properties: ObjectProperty[];
  span: Span;
}

export interface ObjectProperty {
  key: Identifier | StringLiteral | NumericLiteral | Expr;
  value: Expr;
  shorthand?: boolean;
  computed?: boolean;
  span: Span;
}

export interface ListComprehension {
  kind: "ListComprehension";
  expression: Expr;
  targets: Identifier[];  // Changed from single target to array of targets
  iterable: Expr;
  filter?: Expr;
  span: Span;
}

export interface Spread {
  kind: "Spread";
  argument: Expr;
  optional?: boolean;
  span: Span;
}

// Statement nodes
export type Stmt =
  | ExprStmt
  | If
  | Loop
  | Switch
  | Select
  | Match
  | Try
  | Using
  | Defer
  | Break
  | Continue
  | Return
  | Echo
  | Block
  | Throw
  | Yield
  | Go
  | Pass;

export interface ExprStmt {
  kind: "ExprStmt";
  expr: Expr;
  span: Span;
}

export interface If {
  kind: "If";
  arms: IfArm[];
  elseBody?: Block;
  span: Span;
}

export interface IfArm {
  test: Expr;
  body: Block;
  span: Span;
}

export interface Loop {
  kind: "Loop";
  mode: "for" | "while" | "do-while" | "until" | "foreach" | "infinite";
  init?: Stmt | Decl;
  test?: Expr;
  step?: Expr;
  iterable?: Expr;
  variable?: Identifier | ArrayPattern | ObjectPattern;
  iterationKind?: "of" | "in";
  body: Block;
  label?: Identifier;
  await?: boolean;
  span: Span;
}

export interface Switch {
  kind: "Switch";
  discriminant: Expr;
  cases: SwitchCase[];
  defaultCase?: Block;
  span: Span;
}

export interface Select {
  kind: "Select";
  cases: SwitchCase[];
  defaultCase?: Block;
  span: Span;
}

export interface SwitchCase {
  patterns: Expr[];
  guard?: Expr;  // Guard clause for pattern matching (if condition)
  body: Block;
  fallthrough?: boolean;
  span: Span;
}

export interface Match {
  kind: "Match";
  expr: Expr;
  arms: MatchArm[];
  style?: "rust" | "python";
  span: Span;
}

export interface MatchArm {
  patterns: Expr[];
  guard?: Expr;
  body: Expr | Block;
}

export interface Try {
  kind: "Try";
  body: Block;
  catches: CatchClause[];
  finallyBody?: Block;
  span: Span;
}

export interface CatchClause {
  param?: Identifier;
  type?: TypeNode;
  body: Block;
  span: Span;
}

export interface Using {
  kind: "Using";
  resource: Expr | Decl;
  body: Block;
  async?: boolean;
  span: Span;
}

export interface Defer {
  kind: "Defer";
  body: Block | Expr;
  span: Span;
}

export interface Break {
  kind: "Break";
  label?: Identifier;
  span: Span;
}

export interface Continue {
  kind: "Continue";
  label?: Identifier;
  span: Span;
}

export interface Return {
  kind: "Return";
  values: Expr[];
  span: Span;
}

export interface Echo {
  kind: "Echo";
  values: Expr[];
  span: Span;
}

export interface Throw {
  kind: "Throw";
  value: Expr;
  cause?: Expr;
  span: Span;
}

export interface Yield {
  kind: "Yield";
  value?: Expr;
  delegate?: boolean; // for yield*
  span: Span;
}

export interface TypeAssertion {
  kind: "TypeAssertion";
  expr: Expr;
  type: TypeNode;
  span: Span;
}

// JSX Expression nodes
export interface JSXElement {
  kind: "JSXElement";
  openingElement: JSXOpeningElement;
  closingElement: JSXClosingElement | null;
  children: JSXChild[];
  span: Span;
}

export interface JSXFragment {
  kind: "JSXFragment";
  children: JSXChild[];
  span: Span;
}

export interface JSXOpeningElement {
  kind: "JSXOpeningElement";
  name: JSXElementName;
  attributes: JSXAttribute[];
  selfClosing: boolean;
  span: Span;
}

export interface JSXClosingElement {
  kind: "JSXClosingElement";
  name: JSXElementName;
  span: Span;
}

export type JSXElementName = 
  | JSXIdentifier
  | JSXMemberExpression
  | JSXNamespacedName;

export interface JSXIdentifier {
  kind: "JSXIdentifier";
  name: string;
  span: Span;
}

export interface JSXMemberExpression {
  kind: "JSXMemberExpression";
  object: JSXElementName;
  property: JSXIdentifier;
  span: Span;
}

export interface JSXNamespacedName {
  kind: "JSXNamespacedName";
  namespace: JSXIdentifier;
  name: JSXIdentifier;
  span: Span;
}

export type JSXAttribute =
  | JSXNormalAttribute
  | JSXSpreadAttribute;

export interface JSXNormalAttribute {
  kind: "JSXAttribute";
  name: JSXIdentifier | JSXNamespacedName;
  value: JSXAttributeValue | null;
  span: Span;
}

export interface JSXSpreadAttribute {
  kind: "JSXSpreadAttribute";
  argument: Expr;
  span: Span;
}

export type JSXAttributeValue =
  | StringLiteral
  | JSXExpressionContainer
  | JSXElement
  | JSXFragment;

export type JSXChild =
  | JSXText
  | JSXExpressionContainer
  | JSXSpreadChild
  | JSXElement
  | JSXFragment;

export interface JSXText {
  kind: "JSXText";
  value: string;
  raw: string;
  span: Span;
}

export interface JSXExpressionContainer {
  kind: "JSXExpressionContainer";
  expression: Expr | JSXEmptyExpression;
  span: Span;
}

export interface JSXEmptyExpression {
  kind: "JSXEmptyExpression";
  span: Span;
}

export interface JSXSpreadChild {
  kind: "JSXSpreadChild";
  expression: Expr;
  span: Span;
}

export interface RuntimeTag {
  kind: "RuntimeTag";
  runtime: "py" | "js" | "go" | "rb" | "java" | "rs";
  expr: Expr;
  span: Span;
}

export interface Go {
  kind: "Go";
  expr: Expr;
  span: Span;
}

export interface Pass {
  kind: "Pass";
  span: Span;
}

// Declaration nodes
export type Decl =
  | Import
  | GroupedImport
  | ImportDecl
  | VarDecl
  | ConstDecl
  | ShortDecl
  | Reassign
  | FuncDecl
  | TypeDecl
  | ClassDecl
  | InterfaceDecl
  | EnumDecl
  | PackageDecl
  | ExportDecl
  | ImplDecl
  | RustItem;

export interface Import {
  kind: "Import";
  path: string;
  alias?: Identifier;
  span: Span;
}

export interface VarDecl {
  kind: "VarDecl";
  names: Identifier[];
  type?: TypeNode;
  values?: Expr[];
  destructurePattern?: ArrayPattern | ObjectPattern;
  span: Span;
}

export interface ConstDecl {
  kind: "ConstDecl";
  names: Identifier[];
  type?: TypeNode;
  values: Expr[];
  destructurePattern?: ArrayPattern | ObjectPattern;
  span: Span;
}

export interface ShortDecl {
  kind: "ShortDecl";
  targets?: (Identifier | ArrayPattern | ObjectPattern)[];  // For destructuring
  value?: Expr;  // For destructuring
  pairs?: ShortDeclPair[];  // For traditional short decls
  span: Span;
}

export interface ShortDeclPair {
  name: Identifier;
  expr: Expr;
}

export interface Reassign {
  kind: "Reassign";
  name: Identifier;
  expr: Expr;
  span: Span;
}

export interface FuncDecl {
  kind: "FuncDecl";
  name: Identifier;
  genericParams?: Identifier[];
  params: Param[];
  returnType?: TypeNode;
  async?: boolean;
  unsafe?: boolean;
  generator?: boolean;
  decorators?: Decorator[];
  declKeyword?: "def" | "fn" | "fun" | "func" | "function";
  body: Block;
  span: Span;
}

export interface Param {
  name: Identifier | ArrayPattern | ObjectPattern;  // Support destructuring patterns
  type?: TypeNode;
  defaultValue?: Expr;
  visibility?: "public" | "private" | "protected";
  readonly?: boolean;
  spread?: boolean;
  blockParam?: boolean;
  decorators?: Decorator[];
  span: Span;
}

// Destructuring patterns
export interface ArrayPattern {
  kind: "ArrayPattern";
  elements: (Identifier | ArrayPattern | ObjectPattern | ArrayPatternElement | null)[];  // null for holes like [a, , c]
  span: Span;
}

export interface ArrayPatternElement {
  kind: "ArrayPatternElement";
  value: Identifier | ArrayPattern | ObjectPattern;
  rest?: boolean;
  defaultValue?: Expr;
  span: Span;
}

export interface ObjectPattern {
  kind: "ObjectPattern";
  properties: ObjectPatternProperty[];
  span: Span;
}

export interface ObjectPatternProperty {
  key: Identifier;
  value: Identifier | ArrayPattern | ObjectPattern;
  shorthand?: boolean;
  rest?: boolean;
  defaultValue?: Expr;
  span: Span;
}

export interface TypeDecl {
  kind: "TypeDecl";
  name: Identifier;
  genericParams?: Identifier[];
  definition: TypeNode;
  /** True for Rust-style `struct Name { ... }` declarations. */
  structDecl?: boolean;
  span: Span;
}

export interface Decorator {
  kind: "Decorator";
  name: Identifier;
  expression?: Expr;
  args?: Expr[];
  span: Span;
}

export interface ClassDecl {
  kind: "ClassDecl";
  name: Identifier;
  typeParams?: Identifier[];
  genericParams?: Identifier[]; // Alias for typeParams for compatibility
  extends?: TypeNode;
  implements?: TypeNode[];
  members: ClassMember[];
  decorators?: Decorator[];
  span: Span;
}

export interface ClassMember {
  kind: "Field" | "Method" | "Constructor" | "Getter" | "Setter" | "Property";
  name?: Identifier;
  visibility?: "public" | "private" | "protected";
  static?: boolean;
  readonly?: boolean;
  type?: TypeNode;
  params?: Param[];
  body?: Block;
  decorators?: Decorator[];
  // For properties with accessors
  getter?: PropertyAccessor;
  setter?: PropertyAccessor;
  // For preserving unknown modifiers like 'volatile', 'synchronized', etc.
  unknownModifiers?: string[];
  span: Span;
}

export interface PropertyAccessor {
  visibility?: "public" | "private" | "protected";
  body?: Block;
  span: Span;
}

export interface InterfaceDecl {
  kind: "InterfaceDecl";
  name: Identifier;
  typeParams?: Identifier[];
  extends?: TypeNode[];
  members: InterfaceMember[];
  span: Span;
}

export interface InterfaceMember {
  name: Identifier;
  type?: TypeNode;
  optional?: boolean;
  kind?: "Property" | "Method";
  params?: Param[];
  returnType?: TypeNode;
  genericParams?: Identifier[];
  span: Span;
}

export interface EnumDecl {
  kind: "EnumDecl";
  name: Identifier;
  members: EnumMember[];
  /** True when any variant carries a payload (Rust `Name { .. }` / `Name(..)`). */
  payloadVariants?: boolean;
  span: Span;
}

export interface EnumMember {
  name: Identifier;
  value?: Expr;
  span: Span;
}

export interface PackageDecl {
  kind: "PackageDecl";
  name: Identifier;
  span: Span;
}

export interface GroupedImport {
  kind: "GroupedImport";
  imports: Array<Import | ImportDecl>;
  span: Span;
}

export interface ImportDecl {
  kind: "ImportDecl";
  specifiers?: ImportSpecifier[];
  defaultImport?: Identifier;
  namespaceImport?: Identifier;
  path: string;
  span: Span;
}

export interface ImportSpecifier {
  imported: string;
  local: string;
}

export interface ExportDecl {
  kind: "ExportDecl";
  declaration?: Decl;
  specifiers?: ExportSpecifier[];
  source?: string;
  isDefault?: boolean;
  span: Span;
}

export interface ExportSpecifier {
  local: Identifier;
  exported?: Identifier;
  span: Span;
}

/**
 * One opaque Rust item captured verbatim by the raw item scanner
 * (src/rust-item-scanner.ts). The compiler never looks inside `text` except
 * to extract fn signatures; the slice flows verbatim into the shared Rust
 * compilation unit.
 */
export interface RustItem {
  kind: "RustItem";
  itemKind: "fn" | "struct" | "enum" | "union" | "trait" | "impl" | "mod"
    | "use" | "static" | "const" | "type" | "macro" | "extern";
  /** Verbatim source slice, including preceding #[...] / /// lines. */
  text: string;
  /** Primary declared name, when extractable. */
  name?: string;
  /** Top-level fn signatures (exactly one when itemKind === "fn"). */
  fns: Array<{
    name: string;
    async: boolean;
    paramCount: number;
    params: string[];
    /** Raw parameter type texts, parallel to `params` ("" when unknown). */
    paramTypes?: string[];
    /** Raw return type text after `->` ("" when none). */
    returnType?: string;
    /** True when the fn declares non-lifetime generic parameters. */
    typeGenerics?: boolean;
  }>;
  /** Names this item binds at module scope. */
  bindings: string[];
  span: Span;
}

export interface ImplDecl {
  kind: "ImplDecl";
  type: TypeNode;  // The type being implemented for (e.g., Container<T>)
  trait?: TypeNode;  // The trait being implemented (if any)
  typeParams?: Identifier[];  // Generic parameters
  whereClause?: WhereClause;  // Where constraints
  members: ImplMember[];
  span: Span;
}

export interface WhereClause {
  constraints: WhereConstraint[];
  span: Span;
}

export interface WhereConstraint {
  type: TypeNode;  // The type being constrained (e.g., T)
  bounds: TypeNode[];  // The bounds (e.g., [Clone, Send])
  span: Span;
}

export interface ImplMember {
  kind: "Method" | "AssociatedType" | "AssociatedConst" | "Field" | "Unknown";
  name?: Identifier;
  params?: Param[];
  type?: TypeNode;
  body?: Block;
  value?: Expr;
  visibility?: "public" | "private" | "protected";
  isConst?: boolean;
  tokens?: any[]; // For unknown members
  span: Span;
}

// Block node
export interface Block {
  kind: "Block";
  statements: (Decl | Stmt)[];
  span: Span;
}

// Program root
export interface Program {
  kind: "Program";
  body: (Decl | Stmt)[];
  runtimeDirective?: string;
  span: Span;
}
