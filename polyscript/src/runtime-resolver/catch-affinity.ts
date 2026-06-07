import * as AST from '../ast';
import { AffinityEvidence, OmniRuntime, RuntimeAffinity } from './types';

type CatchKeyword = "catch" | "except" | "rescue";

const STANDARD_JAVA_EXCEPTION_TYPES = new Set([
  "Throwable",
  "Exception",
  "RuntimeException",
  "Error",
  "AssertionError",
  "LinkageError",
  "VirtualMachineError",
  "OutOfMemoryError",
  "StackOverflowError",
  "IOException",
  "EOFException",
  "FileNotFoundException",
  "InterruptedIOException",
  "UnsupportedEncodingException",
  "UncheckedIOException",
  "ReflectiveOperationException",
  "ClassNotFoundException",
  "IllegalAccessException",
  "InstantiationException",
  "InvocationTargetException",
  "NoSuchFieldException",
  "NoSuchMethodException",
  "InterruptedException",
  "CloneNotSupportedException",
  "SQLException",
  "SQLTimeoutException",
  "BatchUpdateException",
  "ParseException",
  "ArithmeticException",
  "ArrayIndexOutOfBoundsException",
  "ArrayStoreException",
  "ClassCastException",
  "ConcurrentModificationException",
  "EnumConstantNotPresentException",
  "IllegalArgumentException",
  "IllegalMonitorStateException",
  "IllegalStateException",
  "IndexOutOfBoundsException",
  "NegativeArraySizeException",
  "NoSuchElementException",
  "NullPointerException",
  "NumberFormatException",
  "SecurityException",
  "StringIndexOutOfBoundsException",
  "TypeNotPresentException",
  "UnsupportedOperationException",
]);

export function inferCatchClauseAffinity(
  clause: AST.CatchClause,
  source?: string,
): RuntimeAffinity | undefined {
  const keyword = findCatchKeyword(clause, source);

  if (keyword?.keyword === "except") {
    return catchAffinity(OmniRuntime.Python, "except clause");
  }

  if (keyword?.keyword === "rescue") {
    return catchAffinity(OmniRuntime.Ruby, "rescue clause");
  }

  if (keyword?.keyword === "catch") {
    if (isJavaStyleCatch(clause, source, keyword.index)) {
      return catchAffinity(OmniRuntime.Java, "typed Java catch clause");
    }
    return catchAffinity(OmniRuntime.JavaScript, "catch clause");
  }

  if (clause.type && clause.param && isStandardJavaExceptionType(typeName(clause.type))) {
    return catchAffinity(OmniRuntime.Java, "standard Java exception catch type");
  }

  return undefined;
}

function catchAffinity(runtime: OmniRuntime, detail: string): RuntimeAffinity {
  const evidence: AffinityEvidence = { type: "syntax", detail };
  return { runtime, confidence: "definite", evidence: [evidence] };
}

function findCatchKeyword(
  clause: AST.CatchClause,
  source?: string,
): { keyword: CatchKeyword; index: number } | undefined {
  if (!source) return undefined;

  const anchor = clause.type?.span.start ?? clause.param?.span.start ?? clause.body.span.start;
  const start = Math.max(0, anchor - 160);
  const prefix = source.slice(start, anchor);
  const matches = [...prefix.matchAll(/\b(catch|except|rescue)\b/g)];
  const match = matches[matches.length - 1];
  if (!match) return undefined;

  return {
    keyword: match[1] as CatchKeyword,
    index: start + (match.index ?? 0),
  };
}

function isJavaStyleCatch(
  clause: AST.CatchClause,
  source: string | undefined,
  keywordIndex: number,
): boolean {
  if (!source || !clause.type || !clause.param) return false;

  const betweenKeywordAndType = source.slice(keywordIndex + "catch".length, clause.type.span.start);
  if (betweenKeywordAndType.includes(":")) return false;

  const openingParen = betweenKeywordAndType.lastIndexOf("(");
  if (openingParen === -1) return false;

  return betweenKeywordAndType.slice(openingParen + 1).trim().length === 0;
}

function isStandardJavaExceptionType(name: string | undefined): boolean {
  if (!name) return false;
  if (name.startsWith("java.") || name.startsWith("javax.")) return true;
  const simpleName = name.split(".").pop() ?? name;
  return STANDARD_JAVA_EXCEPTION_TYPES.has(simpleName);
}

function typeName(type: AST.TypeNode): string | undefined {
  switch (type.kind) {
    case "SimpleType":
      return type.id.name;
    case "GenericType":
      return type.base.name;
    default:
      return undefined;
  }
}
