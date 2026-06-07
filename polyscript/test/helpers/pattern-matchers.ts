/**
 * Pattern Matchers for AST Traversal
 * 
 * These functions help find specific patterns in the AST,
 * particularly for angle bracket disambiguation testing.
 */

import * as AST from '../../src/ast';

// ============= Type Guards =============

export function isJSXElement(node: any): node is AST.JSXElement {
  return node && node.kind === 'JSXElement';
}

export function isJSXFragment(node: any): node is AST.JSXFragment {
  return node && node.kind === 'JSXFragment';
}

export function isGenericType(node: any): node is AST.GenericType {
  return node && node.kind === 'GenericType';
}

export function isBinaryOp(node: any): node is AST.Binary {
  return node && node.kind === 'Binary';
}

export function isComparison(node: any): node is AST.Binary {
  return isBinaryOp(node) && ['<', '>', '<=', '>='].includes(node.op);
}

export function isChannelSend(node: any): node is AST.Binary {
  return isBinaryOp(node) && node.op === '<-';
}

export function isChannelReceive(node: any): node is AST.Unary {
  return node && node.kind === 'Unary' && node.op === '<-';
}

// ============= AST Traversal =============

export type NodeVisitor = (node: any, path: string[]) => void;

export function traverseAST(node: any, visitor: NodeVisitor, path: string[] = []) {
  if (!node || typeof node !== 'object') return;
  
  visitor(node, path);
  
  for (const key in node) {
    if (key === 'span' || key === 'loc') continue; // Skip location info
    
    const value = node[key];
    if (value && typeof value === 'object') {
      if (Array.isArray(value)) {
        value.forEach((item, index) => {
          traverseAST(item, visitor, [...path, `${key}[${index}]`]);
        });
      } else {
        traverseAST(value, visitor, [...path, key]);
      }
    }
  }
}

// ============= Pattern Finders =============

export function findJSXElements(ast: AST.Program): AST.JSXElement[] {
  const elements: AST.JSXElement[] = [];
  
  traverseAST(ast, (node) => {
    if (isJSXElement(node)) {
      elements.push(node);
    }
  });
  
  return elements;
}

export function findJSXFragments(ast: AST.Program): AST.JSXFragment[] {
  const fragments: AST.JSXFragment[] = [];
  
  traverseAST(ast, (node) => {
    if (isJSXFragment(node)) {
      fragments.push(node);
    }
  });
  
  return fragments;
}

export function findGenericTypes(ast: AST.Program): AST.GenericType[] {
  const generics: AST.GenericType[] = [];
  
  traverseAST(ast, (node) => {
    if (isGenericType(node)) {
      generics.push(node);
    }
    // Also check for GenericType stored in _typeNode field (for make() calls)
    if ((node as any)?._typeNode && isGenericType((node as any)._typeNode)) {
      generics.push((node as any)._typeNode);
    }
    // Check for genericArgs on Call nodes (e.g., React.forwardRef<T1, T2>())
    if (node && node.kind === 'Call' && node.typeArgs) {
      // Convert genericArgs to GenericType-like structure for compatibility
      const callNode = node as any;
      if (callNode.typeArgs && callNode.typeArgs.length > 0) {
        // Create a synthetic GenericType to represent the call's generic arguments
        const syntheticGeneric: AST.GenericType = {
          kind: 'GenericType',
          base: {
            kind: 'Identifier',
            name: callNode.callee?.property?.name || callNode.callee?.name || 'unknown',
            span: callNode.callee?.span || { start: 0, end: 0, line: 0, column: 0 }
          },
          args: callNode.typeArgs,
          span: callNode.span
        };
        generics.push(syntheticGeneric);
      }
    }
  });
  
  return generics;
}

export function findInterfaces(ast: AST.Program): AST.InterfaceDecl[] {
  const interfaces: AST.InterfaceDecl[] = [];
  
  traverseAST(ast, (node) => {
    if (node && node.kind === 'InterfaceDecl') {
      interfaces.push(node);
    }
  });
  
  return interfaces;
}

export function findComparisons(ast: AST.Program): AST.Binary[] {
  const comparisons: AST.Binary[] = [];
  
  traverseAST(ast, (node) => {
    if (isComparison(node)) {
      comparisons.push(node);
    }
  });
  
  return comparisons;
}

export function findChannelOperations(ast: AST.Program): {
  sends: AST.Binary[];
  receives: AST.Unary[];
} {
  const sends: AST.Binary[] = [];
  const receives: AST.Unary[] = [];
  
  traverseAST(ast, (node) => {
    if (isChannelSend(node)) {
      sends.push(node);
    } else if (isChannelReceive(node)) {
      receives.push(node);
    }
  });
  
  return { sends, receives };
}

// ============= Complex Pattern Matchers =============

export interface AngleBracketUsage {
  jsx: {
    elements: AST.JSXElement[];
    fragments: AST.JSXFragment[];
  };
  generics: AST.GenericType[];
  comparisons: {
    lessThan: AST.Binary[];
    greaterThan: AST.Binary[];
    lessOrEqual: AST.Binary[];
    greaterOrEqual: AST.Binary[];
  };
  channels: {
    sends: AST.Binary[];
    receives: AST.Unary[];
  };
  shifts: {
    leftShift: AST.Binary[];
    rightShift: AST.Binary[];
    unsignedRightShift: AST.Binary[];
  };
}

export function findAllAngleBracketUsages(ast: AST.Program): AngleBracketUsage {
  const result: AngleBracketUsage = {
    jsx: {
      elements: [],
      fragments: []
    },
    generics: [],
    comparisons: {
      lessThan: [],
      greaterThan: [],
      lessOrEqual: [],
      greaterOrEqual: []
    },
    channels: {
      sends: [],
      receives: []
    },
    shifts: {
      leftShift: [],
      rightShift: [],
      unsignedRightShift: []
    }
  };
  
  traverseAST(ast, (node) => {
    // JSX
    if (isJSXElement(node)) {
      result.jsx.elements.push(node);
    } else if (isJSXFragment(node)) {
      result.jsx.fragments.push(node);
    }
    
    // Generics
    else if (isGenericType(node)) {
      result.generics.push(node);
    }
    
    // Binary operators
    else if (isBinaryOp(node)) {
      switch (node.op) {
        case '<':
          result.comparisons.lessThan.push(node);
          break;
        case '>':
          result.comparisons.greaterThan.push(node);
          break;
        case '<=':
          result.comparisons.lessOrEqual.push(node);
          break;
        case '>=':
          result.comparisons.greaterOrEqual.push(node);
          break;
        case '<<':
          result.shifts.leftShift.push(node);
          break;
        case '>>':
          result.shifts.rightShift.push(node);
          break;
        case '>>>':
          result.shifts.unsignedRightShift.push(node);
          break;
        case '<-':
          result.channels.sends.push(node);
          break;
      }
    }
    
    // Unary operators
    else if (node && node.kind === 'Unary' && node.op === '<-') {
      result.channels.receives.push(node as AST.Unary);
    }
  });
  
  return result;
}

// ============= Pattern Analysis =============

export interface AngleBracketStats {
  totalUsages: number;
  jsxCount: number;
  genericCount: number;
  comparisonCount: number;
  channelCount: number;
  shiftCount: number;
  summary: string;
}

export function analyzeAngleBrackets(ast: AST.Program): AngleBracketStats {
  const usage = findAllAngleBracketUsages(ast);
  
  const jsxCount = usage.jsx.elements.length + usage.jsx.fragments.length;
  const genericCount = usage.generics.length;
  const comparisonCount = 
    usage.comparisons.lessThan.length +
    usage.comparisons.greaterThan.length +
    usage.comparisons.lessOrEqual.length +
    usage.comparisons.greaterOrEqual.length;
  const channelCount = usage.channels.sends.length + usage.channels.receives.length;
  const shiftCount = 
    usage.shifts.leftShift.length +
    usage.shifts.rightShift.length +
    usage.shifts.unsignedRightShift.length;
  
  const totalUsages = jsxCount + genericCount + comparisonCount + channelCount + shiftCount;
  
  const parts: string[] = [];
  if (jsxCount > 0) parts.push(`${jsxCount} JSX`);
  if (genericCount > 0) parts.push(`${genericCount} generic`);
  if (comparisonCount > 0) parts.push(`${comparisonCount} comparison`);
  if (channelCount > 0) parts.push(`${channelCount} channel`);
  if (shiftCount > 0) parts.push(`${shiftCount} shift`);
  
  return {
    totalUsages,
    jsxCount,
    genericCount,
    comparisonCount,
    channelCount,
    shiftCount,
    summary: parts.length > 0 ? parts.join(', ') : 'no angle brackets'
  };
}

// ============= Specific Pattern Finders =============

export function findFunctionDeclarations(ast: AST.Program): AST.FuncDecl[] {
  const functions: AST.FuncDecl[] = [];
  
  traverseAST(ast, (node) => {
    if (node && node.kind === 'FuncDecl') {
      functions.push(node);
    }
  });
  
  return functions;
}

export function findVariableDeclarations(ast: AST.Program): AST.VarDecl[] {
  const variables: AST.VarDecl[] = [];
  
  traverseAST(ast, (node) => {
    if (node && node.kind === 'VarDecl') {
      variables.push(node);
    }
  });
  
  return variables;
}

export function findByKind<T = any>(ast: AST.Program, kind: string): T[] {
  const results: T[] = [];
  
  traverseAST(ast, (node) => {
    if (node && node.kind === kind) {
      results.push(node as T);
    }
  });
  
  return results;
}

export function findFirst<T = any>(
  ast: AST.Program, 
  predicate: (node: any) => boolean
): T | null {
  let result: T | null = null;
  
  traverseAST(ast, (node) => {
    if (!result && predicate(node)) {
      result = node as T;
    }
  });
  
  return result;
}

// ============= Export =============

export default {
  // Type guards
  isJSXElement,
  isJSXFragment,
  isGenericType,
  isBinaryOp,
  isComparison,
  isChannelSend,
  isChannelReceive,
  
  // Traversal
  traverseAST,
  
  // Finders
  findJSXElements,
  findJSXFragments,
  findGenericTypes,
  findComparisons,
  findChannelOperations,
  findAllAngleBracketUsages,
  
  // Analysis
  analyzeAngleBrackets,
  
  // Generic finders
  findFunctionDeclarations,
  findVariableDeclarations,
  findByKind,
  findFirst
};