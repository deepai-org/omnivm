/**
 * AST Verification Helpers for PolyScript Tests
 * 
 * These helpers ensure consistent and thorough AST structure verification
 * across all tests, particularly for angle bracket disambiguation.
 */

import * as AST from '../../src/ast';

// ============= JSX Verifiers =============

export function verifyJSXElement(
  node: any,
  expectedTag: string,
  options: {
    selfClosing?: boolean;
    attributeCount?: number;
    childCount?: number;
  } = {}
) {
  expect(node.kind).toBe('JSXElement');
  
  // Verify tag name
  const name = node.openingElement.name;
  if (name.kind === 'JSXIdentifier') {
    expect(name.name).toBe(expectedTag);
  } else if (name.kind === 'JSXMemberExpression') {
    // Handle <Form.Input> style
    const parts = expectedTag.split('.');
    expect(name.object.name).toBe(parts[0]);
    expect(name.property.name).toBe(parts[1]);
  }
  
  // Verify self-closing
  if (options.selfClosing !== undefined) {
    expect(node.openingElement.selfClosing).toBe(options.selfClosing);
  }
  
  // Verify attributes
  if (options.attributeCount !== undefined) {
    expect(node.openingElement.attributes).toHaveLength(options.attributeCount);
  }
  
  // Verify children
  if (options.childCount !== undefined) {
    expect(node.children).toHaveLength(options.childCount);
  }
  
  return node;
}

export function verifyJSXAttribute(
  attr: any,
  name: string,
  valueKind?: 'string' | 'expression' | 'jsx' | 'none'
) {
  expect(attr.kind).toBe('JSXAttribute');
  expect(attr.name.name).toBe(name);
  
  if (valueKind === 'none') {
    expect(attr.value).toBeNull();
  } else if (valueKind === 'string') {
    expect(attr.value.kind).toBe('StringLiteral');
  } else if (valueKind === 'expression') {
    expect(attr.value.kind).toBe('JSXExpressionContainer');
  } else if (valueKind === 'jsx') {
    expect(attr.value.kind).toBe('JSXElement');
  }
  
  return attr;
}

// ============= Generic Type Verifiers =============

export function verifyGenericType(
  node: any,
  baseName: string,
  argCount?: number,
  argVerifiers?: Array<(arg: any) => void>
) {
  expect(node.kind).toBe('GenericType');
  expect(node.base.name).toBe(baseName);
  
  if (argCount !== undefined) {
    expect(node.args).toHaveLength(argCount);
  }
  
  if (argVerifiers) {
    argVerifiers.forEach((verifier, i) => {
      verifier(node.args[i]);
    });
  }
  
  return node;
}

export function verifyNestedGeneric(
  node: any,
  structure: string // e.g., "Result<Vec<T>, Error>"
) {
  // Parse the structure string to verify nested generics
  const match = structure.match(/^(\w+)<(.+)>$/);
  if (!match) throw new Error(`Invalid generic structure: ${structure}`);
  
  const [, baseName, args] = match;
  expect(node.kind).toBe('GenericType');
  expect(node.base.name).toBe(baseName);
  
  // TODO: Parse and verify nested args
  return node;
}

// ============= Operator Verifiers =============

export function verifyBinaryOp(
  node: any,
  op: string,
  leftKind?: string,
  rightKind?: string
) {
  expect(node.kind).toBe('Binary');
  expect(node.op).toBe(op);
  
  if (leftKind) {
    expect(node.left.kind).toBe(leftKind);
  }
  
  if (rightKind) {
    expect(node.right.kind).toBe(rightKind);
  }
  
  return node;
}

export function verifyComparison(
  node: any,
  op: '<' | '>' | '<=' | '>=',
  left?: string | number,
  right?: string | number
) {
  verifyBinaryOp(node, op);
  
  if (left !== undefined) {
    if (typeof left === 'string') {
      expect(node.left.name).toBe(left);
    } else {
      expect(node.left.raw).toBe(String(left));
    }
  }
  
  if (right !== undefined) {
    if (typeof right === 'string') {
      expect(node.right.name).toBe(right);
    } else {
      expect(node.right.raw).toBe(String(right));
    }
  }
  
  return node;
}

// ============= Channel Operation Verifiers =============

export function verifyChannelSend(node: any, channel?: string, value?: string) {
  expect(node.kind).toBe('Binary');
  expect(node.op).toBe('<-');
  
  if (channel) {
    expect(node.left.name).toBe(channel);
  }
  
  if (value) {
    if (node.right.kind === 'Identifier') {
      expect(node.right.name).toBe(value);
    }
  }
  
  return node;
}

export function verifyChannelReceive(node: any, channel?: string) {
  expect(node.kind).toBe('Unary');
  expect(node.op).toBe('<-');
  
  // Handle both operand and argument properties
  const operand = node.operand || node.argument;
  if (channel && operand && operand.kind === 'Identifier') {
    expect(operand.name).toBe(channel);
  }
  
  return node;
}

// ============= AST Search Helpers =============

export function findInAST(
  node: any,
  predicate: (node: any) => boolean
): any | null {
  if (predicate(node)) return node;
  
  for (const key in node) {
    const value = node[key];
    if (value && typeof value === 'object') {
      if (Array.isArray(value)) {
        for (const item of value) {
          const found = findInAST(item, predicate);
          if (found) return found;
        }
      } else {
        const found = findInAST(value, predicate);
        if (found) return found;
      }
    }
  }
  
  return null;
}

export function findAllInAST(
  node: any,
  predicate: (node: any) => boolean
): any[] {
  const results: any[] = [];
  
  function traverse(n: any) {
    if (predicate(n)) results.push(n);
    
    for (const key in n) {
      const value = n[key];
      if (value && typeof value === 'object') {
        if (Array.isArray(value)) {
          value.forEach(traverse);
        } else {
          traverse(value);
        }
      }
    }
  }
  
  traverse(node);
  return results;
}

// ============= Compound Verifiers =============

export interface AngleBracketExpectations {
  jsx?: Array<{ tag: string; selfClosing?: boolean }>;
  generics?: Array<{ base: string; argCount: number }>;
  comparisons?: Array<{ op: string; left?: string; right?: string | number }>;
  channels?: {
    sends?: number;
    receives?: number;
  };
}

export function verifyAngleBrackets(
  ast: AST.Program,
  expectations: AngleBracketExpectations
) {
  // Find all JSX elements
  if (expectations.jsx) {
    const jsxElements = findAllInAST(ast, n => n.kind === 'JSXElement');
    expect(jsxElements).toHaveLength(expectations.jsx.length);
    
    expectations.jsx.forEach((expected, i) => {
      verifyJSXElement(jsxElements[i], expected.tag, {
        selfClosing: expected.selfClosing
      });
    });
  }
  
  // Find all generic types
  if (expectations.generics) {
    const genericTypes = findAllInAST(ast, n => n.kind === 'GenericType');
    expect(genericTypes).toHaveLength(expectations.generics.length);
    
    expectations.generics.forEach((expected, i) => {
      verifyGenericType(genericTypes[i], expected.base, expected.argCount);
    });
  }
  
  // Find all comparisons
  if (expectations.comparisons) {
    const comparisons = findAllInAST(ast, n => 
      n.kind === 'Binary' && ['<', '>', '<=', '>='].includes(n.op)
    );
    expect(comparisons).toHaveLength(expectations.comparisons.length);
    
    expectations.comparisons.forEach((expected, i) => {
      verifyComparison(comparisons[i], expected.op as any, expected.left, expected.right);
    });
  }
  
  // Find channel operations
  if (expectations.channels) {
    if (expectations.channels.sends !== undefined) {
      const sends = findAllInAST(ast, n => 
        n.kind === 'Binary' && n.op === '<-'
      );
      expect(sends).toHaveLength(expectations.channels.sends);
    }
    
    if (expectations.channels.receives !== undefined) {
      const receives = findAllInAST(ast, n => 
        n.kind === 'Unary' && n.op === '<-'
      );
      expect(receives).toHaveLength(expectations.channels.receives);
    }
  }
}

// ============= Function/Class Verifiers =============

export function verifyFunctionDecl(
  node: any,
  name: string,
  options: {
    async?: boolean;
    genericParams?: string[];
    paramCount?: number;
    returnType?: (type: any) => void;
  } = {}
) {
  expect(node.kind).toBe('FuncDecl');
  expect(node.name.name).toBe(name);
  
  if (options.async !== undefined) {
    expect(node.async).toBe(options.async);
  }
  
  if (options.genericParams) {
    expect(node.genericParams).toHaveLength(options.genericParams.length);
    options.genericParams.forEach((param, i) => {
      expect(node.genericParams[i].name).toBe(param);
    });
  }
  
  if (options.paramCount !== undefined) {
    expect(node.params).toHaveLength(options.paramCount);
  }
  
  if (options.returnType) {
    options.returnType(node.returnType);
  }
  
  return node;
}

export function verifyInterfaceDecl(
  node: any,
  name: string,
  options: {
    propertyCount?: number;
  } = {}
) {
  expect(node.kind).toBe('InterfaceDecl');
  expect(node.name.name).toBe(name);
  
  if (options.propertyCount !== undefined) {
    const properties = node.properties || node.members;
    expect(properties).toHaveLength(options.propertyCount);
  }
  
  return node;
}

export function verifyJSXFragment(node: any, childCount?: number) {
  expect(node.kind).toBe('JSXFragment');
  
  if (childCount !== undefined) {
    expect(node.children).toHaveLength(childCount);
  }
  
  return node;
}

// Utility helpers
export function findFirst<T = any>(
  node: any,
  predicate: (node: any) => boolean
): T | null {
  return findInAST(node, predicate) as T | null;
}

export function findByKind<T = any>(
  node: any,
  kind: string
): T[] {
  return findAllInAST(node, n => n.kind === kind) as T[];
}

// ============= Export all verifiers =============

export default {
  // JSX
  verifyJSXElement,
  verifyJSXFragment,
  verifyJSXAttribute,
  
  // Generics
  verifyGenericType,
  verifyNestedGeneric,
  
  // Operators
  verifyBinaryOp,
  verifyComparison,
  
  // Channels
  verifyChannelSend,
  verifyChannelReceive,
  
  // Search
  findInAST,
  findAllInAST,
  findFirst,
  findByKind,
  
  // Compound
  verifyAngleBrackets,
  
  // Functions/Classes
  verifyFunctionDecl,
  verifyInterfaceDecl
};