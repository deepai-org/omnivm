/**
 * AST Compatibility Layer
 * 
 * This module provides compatibility helpers to handle differences
 * between expected and actual AST structure.
 */

import * as AST from '../../src/ast';

// Fix Call node differences (callee vs func)
export function getCallTarget(call: any): any {
  return call.func || call.callee;
}

// Fix Unary node differences (operand vs argument)
export function getUnaryOperand(unary: any): any {
  return unary.operand || unary.argument;
}

// Check if a node is a Yield expression
export function isYieldExpr(node: any): boolean {
  return node.kind === 'Yield';
}

// Check if a node is an Export declaration
export function isExportDecl(node: any): boolean {
  return node.kind === 'ExportDecl';
}

// Check if a node is a Package declaration
export function isPackageDecl(node: any): boolean {
  return node.kind === 'PackageDecl';
}

// Check if a node is a Go statement
export function isGoStmt(node: any): boolean {
  return node.kind === 'Go';
}

// Check if a node is a Throw statement
export function isThrowStmt(node: any): boolean {
  return node.kind === 'Throw';
}

// Convert GenericType with chan base to ChanType for compatibility
export function normalizeChannelType(type: any): any {
  if (type.kind === 'GenericType' && type.base.name === 'chan') {
    // Convert GenericType chan<T> to ChanType for backward compatibility
    return {
      kind: 'ChanType',
      direction: 'both',
      elementType: type.args[0],
      span: type.span
    };
  }
  return type;
}

// Get return value (handles value vs values)
export function getReturnValue(returnStmt: any): any {
  // Support both single value and values array
  if (returnStmt.value !== undefined) {
    return returnStmt.value;
  }
  if (returnStmt.values && returnStmt.values.length === 1) {
    return returnStmt.values[0];
  }
  return returnStmt.values;
}

// Normalize type declaration kinds
export function normalizeTypeDecl(node: any): any {
  if (node.kind === 'TypeDecl') {
    return { ...node, kind: 'TypeAlias' };
  }
  return node;
}

// Normalize variable declaration kinds
export function normalizeVarDecl(node: any): any {
  if (node.kind === 'ShortDecl') {
    // Convert ShortDecl to VarDecl format
    return {
      ...node,
      kind: 'VarDecl',
      names: node.pairs.map((p: any) => p.name),
      values: node.pairs.map((p: any) => p.expr)
    };
  }
  return node;
}

// Normalize interface declaration (members vs properties)
export function normalizeInterfaceDecl(node: any): any {
  if (node.kind === 'InterfaceDecl' && node.members) {
    return {
      ...node,
      properties: node.members
    };
  }
  return node;
}

// Get IfArm condition (handles test vs condition)
export function getIfCondition(arm: any): any {
  return arm.condition || arm.test;
}

// Get the actual expression from various statement types
export function getExpression(stmt: any): any {
  if (stmt.kind === 'ExprStmt') {
    return stmt.expr;
  } else if (stmt.kind === 'Go') {
    return stmt.expr || stmt.call;
  } else if (stmt.kind === 'Throw') {
    return stmt.value || stmt.argument;
  }
  return stmt;
}

// Check if function has export modifier
export function isFunctionExported(func: any): boolean {
  return func.exported || func.export || false;
}

// Safe property access helpers
export function hasProperty(obj: any, prop: string): boolean {
  return obj && typeof obj === 'object' && prop in obj;
}

export function getProperty(obj: any, prop: string, defaultValue: any = undefined): any {
  return hasProperty(obj, prop) ? obj[prop] : defaultValue;
}

// Node kind checkers with fallbacks
export function isNodeKind(node: any, ...kinds: string[]): boolean {
  return node && kinds.includes(node.kind);
}

// Enhanced channel operation checks
export function isChannelSend(node: any): boolean {
  return node && node.kind === 'Binary' && node.op === '<-';
}

export function isChannelReceive(node: any): boolean {
  return node && node.kind === 'Unary' && node.op === '<-';
}

// Get channel operand with compatibility
export function getChannelOperand(node: any): any {
  if (node.kind === 'Unary' && node.op === '<-') {
    return node.argument || node.operand;
  }
  return null;
}

// Type-safe node casting with validation
export function asNode<T>(node: any, kind: string): T | null {
  if (node && node.kind === kind) {
    return node as T;
  }
  return null;
}

// Find nodes with compatibility checks
export function findCompatibleNodes(ast: any, predicate: (node: any) => boolean): any[] {
  const results: any[] = [];
  
  function traverse(node: any) {
    if (!node || typeof node !== 'object') return;
    
    if (predicate(node)) {
      results.push(node);
    }
    
    for (const key in node) {
      if (key === 'span' || key === 'loc') continue;
      const value = node[key];
      if (Array.isArray(value)) {
        value.forEach(traverse);
      } else if (value && typeof value === 'object') {
        traverse(value);
      }
    }
  }
  
  traverse(ast);
  return results;
}

// Conditional/Ternary compatibility
export function isConditionalExpr(node: any): boolean {
  return node && (node.kind === 'Conditional' || node.kind === 'Ternary');
}

// Map Ternary nodes to look like Conditional for tests
export function normalizeConditional(node: any): any {
  if (!node) return node;
  if (node.kind === 'Ternary') {
    return {
      ...node,
      kind: 'Conditional',
      condition: node.test,
      then: node.consequent,
      else: node.alternate
    };
  }
  return node;
}

// SwitchCase compatibility - add value property
export function normalizeSwitchCase(caseNode: any): any {
  if (!caseNode) return caseNode;
  // If patterns array exists and has elements, use first as value
  if (caseNode.patterns && caseNode.patterns.length > 0) {
    return {
      ...caseNode,
      value: caseNode.patterns[0]
    };
  }
  return caseNode;
}

// Loop compatibility - add condition property
export function normalizeLoop(loop: any): any {
  if (!loop) return loop;
  // Map test to condition for compatibility
  if (loop.test && !loop.condition) {
    return {
      ...loop,
      condition: loop.test
    };
  }
  return loop;
}

// ClassDecl compatibility - ensure genericParams exists
export function normalizeClassDecl(cls: any): any {
  if (!cls) return cls;
  // Add genericParams if missing but typeParams exists
  if (!cls.genericParams && cls.typeParams) {
    return {
      ...cls,
      genericParams: cls.typeParams
    };
  }
  // Also add extends from superClass
  if (!cls.extends && cls.superClass) {
    return {
      ...cls,
      genericParams: cls.typeParams,
      extends: cls.superClass
    };
  }
  return cls;
}

// Try statement compatibility - map finalizer to finally
export function normalizeTryStmt(stmt: any): any {
  if (!stmt) return stmt;
  if (stmt.finalizer && !stmt.finally) {
    return {
      ...stmt,
      finally: stmt.finalizer
    };
  }
  return stmt;
}

// Switch statement compatibility - map discriminant to expr and merge defaultCase
export function normalizeSwitchStmt(stmt: any): any {
  if (!stmt) return stmt;
  
  let result = stmt;
  
  // Map discriminant to expr if needed
  if (stmt.discriminant && !stmt.expr) {
    result = {
      ...result,
      expr: stmt.discriminant
    };
  }
  
  // If there's a separate defaultCase, merge it into cases array
  if (stmt.defaultCase && stmt.cases) {
    const cases = [...stmt.cases];
    // Add default case to the array with isDefault flag
    // The defaultCase is already a Block, so use it as the body
    cases.push({
      isDefault: true,
      value: null,
      patterns: [],
      body: stmt.defaultCase // defaultCase is already a Block with statements
    });
    result = {
      ...result,
      cases
    };
  }
  
  return result;
}

// Export all compatibility helpers
export default {
  getCallTarget,
  getUnaryOperand,
  isYieldExpr,
  isExportDecl,
  isPackageDecl,
  isGoStmt,
  isThrowStmt,
  getExpression,
  isFunctionExported,
  hasProperty,
  getProperty,
  isNodeKind,
  isChannelSend,
  isChannelReceive,
  getChannelOperand,
  asNode,
  findCompatibleNodes,
  isConditionalExpr,
  normalizeConditional,
  normalizeSwitchCase,
  normalizeLoop,
  normalizeClassDecl,
  normalizeTryStmt,
  normalizeSwitchStmt
};