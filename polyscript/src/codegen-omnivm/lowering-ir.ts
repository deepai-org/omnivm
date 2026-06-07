import * as AST from '../ast';
import { BridgeDescriptor, OmniRuntime } from '../runtime-resolver/types';

export type LoweredManifestNode =
  | LoweredEvalExpr
  | LoweredExecStmt
  | LoweredImport
  | LoweredDefineFunc
  | LoweredCallRuntimeFunc
  | LoweredSpawn
  | LoweredChannelOp
  | LoweredBridgeValue;

export interface NativePayload {
  source: string;
  span: AST.Span;
}

export interface NativeDependency {
  name: string;
  argc: number;
}

export interface LoweredGoParam {
  name: string;
  type: string;
}

export interface LoweredGoFuncArtifact {
  exportName: string;
  params: LoweredGoParam[];
  returnType: string;
  signature: string;
  bodyLines: string[];
  imports: string[];
  helperSources: string[];
  dependencies: NativeDependency[];
  varDecls: string[];
  source: string;
}

export interface LoweredFuncSourceArtifact {
  paramsSource: string[];
  bodySource: string;
  functionSource: string;
}

export interface LoweredImportArtifact {
  path: string;
  bind?: string;
  defaultImport?: string;
  namespaceImport?: string;
  specifiers?: Array<{ imported: string; local: string }>;
  source: string;
}

export interface LoweredBase {
  id: number;
  runtime: OmniRuntime;
  sourceNode: AST.Program | AST.Decl | AST.Stmt | AST.Expr;
  native?: NativePayload;
}

export interface LoweredEvalExpr extends LoweredBase {
  kind: "EvalExpr";
  bind?: string;
  expr: AST.Expr;
}

export interface LoweredExecStmt extends LoweredBase {
  kind: "ExecStmt";
  node: AST.Decl | AST.Stmt | AST.Expr;
}

export interface LoweredImport extends LoweredBase {
  kind: "Import";
  artifact: LoweredImportArtifact;
}

export interface LoweredDefineFunc extends LoweredBase {
  kind: "DefineFunc";
  name: string;
  params: string[];
  bodyRuntime: OmniRuntime;
  sourceArtifact?: LoweredFuncSourceArtifact;
  dependencies?: NativeDependency[];
  go?: LoweredGoFuncArtifact;
}

export interface LoweredCallRuntimeFunc extends LoweredBase {
  kind: "CallRuntimeFunc";
  callee: string;
  args: AST.Expr[];
  expr: AST.Call;
  bind?: string;
}

export interface LoweredSpawn extends LoweredBase {
  kind: "Spawn";
  bind?: string;
  expr: AST.Go;
}

export type ChannelAction = "make" | "send" | "recv" | "close";

export interface LoweredChannelOp extends LoweredBase {
  kind: "ChannelMake" | "ChannelSend" | "ChannelRecv" | "ChannelClose";
  action: ChannelAction;
  channel?: string;
  size?: number;
  value?: AST.Expr;
  bind?: string;
}

export interface LoweredBridgeValue extends LoweredBase {
  kind: "BridgeValue";
  from: OmniRuntime;
  to: OmniRuntime;
  marshalKind: BridgeDescriptor["marshalKind"];
}

export interface LoweredManifestIR {
  version: 1;
  defaultRuntime: OmniRuntime;
  nodes: LoweredManifestNode[];
}
