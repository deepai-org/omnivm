/**
 * PolyScript to TypeScript Transpiler
 * 
 * Converts PolyScript AST to TypeScript source code
 */

import * as AST from './ast';

export interface TranspilerOptions {
  indent?: string;
  preserveComments?: boolean;
  targetES?: 'ES2015' | 'ES2020' | 'ES2022' | 'ESNext';
}

export class Transpiler {
  private options: Required<TranspilerOptions>;
  private indentLevel: number = 0;
  private output: string = '';

  constructor(options: TranspilerOptions = {}) {
    this.options = {
      indent: options.indent ?? '  ',
      preserveComments: options.preserveComments ?? true,
      targetES: options.targetES ?? 'ES2022'
    };
  }

  transpile(ast: AST.Program): string {
    this.output = '';
    this.indentLevel = 0;
    this.visitProgram(ast);
    return this.output;
  }

  private emit(text: string) {
    this.output += text;
  }

  private emitLine(text: string = '') {
    if (text) {
      this.emit(this.options.indent.repeat(this.indentLevel) + text);
    }
    this.emit('\n');
  }

  private indent() {
    this.indentLevel++;
  }

  private dedent() {
    this.indentLevel--;
  }

  private visitProgram(node: AST.Program) {
    for (const item of node.body) {
      this.visitNode(item);
    }
  }

  private visitNode(node: AST.Decl | AST.Stmt | AST.Expr | null): void {
    if (!node) return;

    switch (node.kind) {
      // Declarations
      case 'VarDecl':
        this.visitVarDecl(node as AST.VarDecl);
        break;
      case 'ConstDecl':
        this.visitConstDecl(node as AST.ConstDecl);
        break;
      case 'ShortDecl':
        this.visitShortDecl(node as AST.ShortDecl);
        break;
      case 'FuncDecl':
        this.visitFuncDecl(node as AST.FuncDecl);
        break;
      case 'ClassDecl':
        this.visitClassDecl(node as AST.ClassDecl);
        break;
      case 'InterfaceDecl':
        this.visitInterfaceDecl(node as AST.InterfaceDecl);
        break;
      case 'TypeDecl':
        this.visitTypeDecl(node as AST.TypeDecl);
        break;
      case 'Import':
        this.visitImport(node as AST.Import);
        break;
      case 'ExportDecl':
        this.visitExportDecl(node as AST.ExportDecl);
        break;

      // Statements
      case 'ExprStmt':
        this.visitExprStmt(node as AST.ExprStmt);
        break;
      case 'If':
        this.visitIf(node as AST.If);
        break;
      case 'Loop':
        this.visitLoop(node as AST.Loop);
        break;
      case 'Switch':
        this.visitSwitch(node as AST.Switch);
        break;
      case 'Try':
        this.visitTry(node as AST.Try);
        break;
      case 'Return':
        this.visitReturn(node as AST.Return);
        break;
      case 'Break':
        this.visitBreak(node as AST.Break);
        break;
      case 'Continue':
        this.visitContinue(node as AST.Continue);
        break;
      case 'Throw':
        this.visitThrow(node as AST.Throw);
        break;
      case 'Block':
        this.visitBlock(node as AST.Block);
        break;

      // Expressions
      case 'Identifier':
        this.emit(this.visitIdentifier(node as AST.Identifier));
        break;
      case 'NumericLiteral':
        this.emit(this.visitNumericLiteral(node as AST.NumericLiteral));
        break;
      case 'StringLiteral':
        this.emit(this.visitStringLiteral(node as AST.StringLiteral));
        break;
      case 'BooleanLiteral':
        this.emit(this.visitBooleanLiteral(node as AST.BooleanLiteral));
        break;
      case 'NullLiteral':
        this.emit('null');
        break;
      case 'ArrayLiteral':
        this.emit(this.visitArrayLiteral(node as AST.ArrayLiteral));
        break;
      case 'ObjectLiteral':
        this.emit(this.visitObjectLiteral(node as AST.ObjectLiteral));
        break;
      case 'Lambda':
        this.emit(this.visitLambda(node as AST.Lambda));
        break;
      case 'Call':
        this.emit(this.visitCall(node as AST.Call));
        break;
      case 'Member':
        this.emit(this.visitMember(node as AST.Member));
        break;
      case 'Index':
        this.emit(this.visitIndex(node as AST.Index));
        break;
      case 'Unary':
        this.emit(this.visitUnary(node as AST.Unary));
        break;
      case 'Binary':
        this.emit(this.visitBinary(node as AST.Binary));
        break;
      case 'Assign':
        this.emit(this.visitAssign(node as AST.Assign));
        break;
      case 'Ternary':
        this.emit(this.visitTernary(node as AST.Ternary));
        break;
      case 'Yield':
        this.emit(this.visitYield(node as AST.Yield));
        break;
      case 'Spread':
        this.emit(this.visitSpread(node as AST.Spread));
        break;
      case 'TypeAssertion':
        this.emit(this.visitTypeAssertion(node as AST.TypeAssertion));
        break;

      default:
        // For unhandled nodes, emit a comment
        this.emitLine(`// TODO: Unhandled node kind: ${node.kind}`);
    }
  }

  // Declarations

  private visitVarDecl(node: AST.VarDecl) {
    const keyword = 'let';
    this.emit(`${keyword} `);
    
    for (let i = 0; i < node.names.length; i++) {
      if (i > 0) this.emit(', ');
      this.emit(this.visitIdentifier(node.names[i]));
      
      if (node.type) {
        this.emit(': ');
        this.emit(this.visitType(node.type));
      }
    }
    
    if (node.values && node.values.length > 0) {
      this.emit(' = ');
      if (node.values.length === 1) {
        this.emit(this.visitExpression(node.values[0]));
      } else {
        this.emit('[');
        for (let i = 0; i < node.values.length; i++) {
          if (i > 0) this.emit(', ');
          this.emit(this.visitExpression(node.values[i]));
        }
        this.emit(']');
      }
    }
    
    this.emitLine(';');
  }

  private visitConstDecl(node: AST.ConstDecl) {
    this.emit('const ');
    
    for (let i = 0; i < node.names.length; i++) {
      if (i > 0) this.emit(', ');
      this.emit(this.visitIdentifier(node.names[i]));
      
      if (node.type) {
        this.emit(': ');
        this.emit(this.visitType(node.type));
      }
    }
    
    if (node.values && node.values.length > 0) {
      this.emit(' = ');
      if (node.values.length === 1) {
        this.emit(this.visitExpression(node.values[0]));
      } else {
        this.emit('[');
        for (let i = 0; i < node.values.length; i++) {
          if (i > 0) this.emit(', ');
          this.emit(this.visitExpression(node.values[i]));
        }
        this.emit(']');
      }
    }
    
    this.emitLine(';');
  }

  private visitShortDecl(node: AST.ShortDecl) {
    // Convert PolyScript's := to TypeScript const
    if (node.pairs) {
      // Traditional short declaration
      for (let i = 0; i < node.pairs.length; i++) {
        if (i > 0) this.emitLine(';');
        this.emit('const ');
        this.emit(this.visitIdentifier(node.pairs[i].name));
        this.emit(' = ');
        this.emit(this.visitExpression(node.pairs[i].expr));
      }
    } else if (node.targets && node.value) {
      // Destructuring short declaration
      this.emit('const ');
      
      if (node.targets.length === 1) {
        // Single target
        const target = node.targets[0];
        if (target.kind === 'Identifier') {
          this.emit(this.visitIdentifier(target));
        } else if (target.kind === 'ArrayPattern') {
          this.emit(this.visitArrayPattern(target));
        } else if (target.kind === 'ObjectPattern') {
          this.emit(this.visitObjectPattern(target));
        }
      } else {
        // Multiple targets - need to handle specially
        this.emit('[');
        for (let i = 0; i < node.targets.length; i++) {
          if (i > 0) this.emit(', ');
          const target = node.targets[i];
          if (target.kind === 'Identifier') {
            this.emit(this.visitIdentifier(target));
          } else if (target.kind === 'ArrayPattern') {
            this.emit(this.visitArrayPattern(target));
          } else if (target.kind === 'ObjectPattern') {
            this.emit(this.visitObjectPattern(target));
          }
        }
        this.emit(']');
      }
      
      this.emit(' = ');
      this.emit(this.visitExpression(node.value));
    }
    this.emitLine(';');
  }

  private visitFuncDecl(node: AST.FuncDecl) {
    if (node.async) this.emit('async ');
    this.emit('function ');
    if (node.generator) this.emit('* ');
    this.emit(this.visitIdentifier(node.name));
    
    this.emit('(');
    for (let i = 0; i < node.params.length; i++) {
      if (i > 0) this.emit(', ');
      this.visitParam(node.params[i]);
    }
    this.emit(')');
    
    if (node.returnType) {
      this.emit(': ');
      this.emit(this.visitType(node.returnType));
    }
    
    this.emit(' ');
    this.visitBlock(node.body);
  }

  private visitClassDecl(node: AST.ClassDecl) {
    this.emit('class ');
    this.emit(this.visitIdentifier(node.name));
    
    if (node.typeParams && node.typeParams.length > 0) {
      this.emit('<');
      for (let i = 0; i < node.typeParams.length; i++) {
        if (i > 0) this.emit(', ');
        this.emit(this.visitIdentifier(node.typeParams[i]));
      }
      this.emit('>');
    }
    
    if (node.extends) {
      this.emit(' extends ');
      this.emit(this.visitType(node.extends));
    }
    
    if (node.implements && node.implements.length > 0) {
      this.emit(' implements ');
      for (let i = 0; i < node.implements.length; i++) {
        if (i > 0) this.emit(', ');
        this.emit(this.visitType(node.implements[i]));
      }
    }
    
    this.emitLine(' {');
    this.indent();
    
    for (const member of node.members) {
      this.visitClassMember(member);
    }
    
    this.dedent();
    this.emitLine('}');
  }

  private visitInterfaceDecl(node: AST.InterfaceDecl) {
    this.emit('interface ');
    this.emit(this.visitIdentifier(node.name));
    
    if (node.typeParams && node.typeParams.length > 0) {
      this.emit('<');
      for (let i = 0; i < node.typeParams.length; i++) {
        if (i > 0) this.emit(', ');
        this.emit(this.visitIdentifier(node.typeParams[i]));
      }
      this.emit('>');
    }
    
    if (node.extends && node.extends.length > 0) {
      this.emit(' extends ');
      for (let i = 0; i < node.extends.length; i++) {
        if (i > 0) this.emit(', ');
        this.emit(this.visitType(node.extends[i]));
      }
    }
    
    this.emitLine(' {');
    this.indent();
    
    for (const member of node.members) {
      this.visitInterfaceMember(member);
    }
    
    this.dedent();
    this.emitLine('}');
  }

  private visitTypeDecl(node: AST.TypeDecl) {
    this.emit('type ');
    this.emit(this.visitIdentifier(node.name));
    this.emit(' = ');
    this.emit(this.visitType(node.definition));
    this.emitLine(';');
  }

  private visitImport(node: AST.Import) {
    this.emit('import ');
    if (node.alias) {
      this.emit(this.visitIdentifier(node.alias));
      this.emit(' from ');
    }
    this.emit(`'${node.path}'`);
    this.emitLine(';');
  }

  private visitExportDecl(node: AST.ExportDecl) {
    this.emit('export ');
    
    if (node.declaration) {
      this.visitNode(node.declaration);
    } else if (node.specifiers) {
      this.emit('{ ');
      for (let i = 0; i < node.specifiers.length; i++) {
        if (i > 0) this.emit(', ');
        const spec = node.specifiers[i];
        this.emit(this.visitIdentifier(spec.local));
        if (spec.exported && spec.exported.name !== spec.local.name) {
          this.emit(' as ');
          this.emit(this.visitIdentifier(spec.exported));
        }
      }
      this.emit(' }');
      
      if (node.source) {
        this.emit(' from ');
        this.emit(`'${node.source}'`);
      }
      
      this.emitLine(';');
    }
  }

  // Statements

  private visitExprStmt(node: AST.ExprStmt) {
    this.emit(this.visitExpression(node.expr));
    this.emitLine(';');
  }

  private visitIf(node: AST.If) {
    if (node.arms && node.arms.length > 0) {
      for (let i = 0; i < node.arms.length; i++) {
        if (i > 0) this.emit(' else ');
        this.emit('if (');
        this.emit(this.visitExpression(node.arms[i].test));
        this.emit(') ');
        this.visitBlock(node.arms[i].body);
      }
    }
    
    if (node.elseBody) {
      this.emit(' else ');
      this.visitBlock(node.elseBody);
    }
  }

  private visitLoop(node: AST.Loop) {
    if (node.mode === 'while' || node.mode === 'until') {
      const op = node.mode === 'until' ? '!' : '';
      this.emit(`while (${op}`);
      this.emit(node.test ? this.visitExpression(node.test) : 'true');
      this.emit(') ');
      this.visitBlock(node.body);
    } else if (node.mode === 'for') {
      this.emit('for (');
      
      if (node.init) {
        if (node.init.kind === 'VarDecl' || node.init.kind === 'ConstDecl') {
          const decl = node.init as AST.VarDecl | AST.ConstDecl;
          const keyword = decl.kind === 'VarDecl' ? 'let' : 'const';
          this.emit(`${keyword} `);
          for (let i = 0; i < decl.names.length; i++) {
            if (i > 0) this.emit(', ');
            this.emit(this.visitIdentifier(decl.names[i]));
          }
          if (decl.values && decl.values.length > 0) {
            this.emit(' = ');
            this.emit(this.visitExpression(decl.values[0]));
          }
        } else {
          this.emit(this.visitExpression(node.init as AST.Expr));
        }
      }
      this.emit('; ');
      
      if (node.test) {
        this.emit(this.visitExpression(node.test));
      }
      this.emit('; ');
      
      if (node.step) {
        this.emit(this.visitExpression(node.step));
      }
      
      this.emit(') ');
      this.visitBlock(node.body);
    } else if (node.mode === 'foreach') {
      this.emit('for (');
      if (node.await) this.emit('await ');
      this.emit('const ');
      if (node.variable) {
        if (node.variable.kind === "ArrayPattern") {
          const names = node.variable.elements
            .filter((e): e is AST.Identifier => e !== null && e.kind === "Identifier")
            .map(e => this.visitIdentifier(e));
          this.emit('[' + names.join(', ') + ']');
        } else {
          if (node.variable.kind === "Identifier") {
            this.emit(this.visitIdentifier(node.variable));
          } else {
            this.emit('/* pattern */');
          }
        }
      }
      this.emit(' of ');
      if (node.iterable) {
        this.emit(this.visitExpression(node.iterable));
      }
      this.emit(') ');
      this.visitBlock(node.body);
    } else if (node.mode === 'infinite') {
      this.emit('while (true) ');
      this.visitBlock(node.body);
    }
  }

  private visitSwitch(node: AST.Switch) {
    this.emit('switch (');
    this.emit(this.visitExpression(node.discriminant));
    this.emitLine(') {');
    this.indent();
    
    for (const caseNode of node.cases) {
      if (caseNode.patterns && caseNode.patterns.length > 0) {
        for (const pattern of caseNode.patterns) {
          this.emit('case ');
          this.emit(this.visitExpression(pattern));
          this.emitLine(':');
        }
      }
      
      this.indent();
      this.visitBlock(caseNode.body);
      if (!caseNode.fallthrough) {
        this.emitLine('break;');
      }
      this.dedent();
    }
    
    if (node.defaultCase) {
      this.emitLine('default:');
      this.indent();
      this.visitBlock(node.defaultCase);
      this.dedent();
    }
    
    this.dedent();
    this.emitLine('}');
  }

  private visitTry(node: AST.Try) {
    this.emit('try ');
    this.visitBlock(node.body);
    
    if (node.catches && node.catches.length > 0) {
      for (const catchClause of node.catches) {
        this.emit(' catch');
        if (catchClause.param) {
          this.emit(' (');
          this.emit(this.visitIdentifier(catchClause.param));
          if (catchClause.type) {
            this.emit(': ');
            this.emit(this.visitType(catchClause.type));
          }
          this.emit(')');
        }
        this.emit(' ');
        this.visitBlock(catchClause.body);
      }
    }
    
    if (node.finallyBody) {
      this.emit(' finally ');
      this.visitBlock(node.finallyBody);
    }
  }

  private visitReturn(node: AST.Return) {
    this.emit('return');
    if (node.values && node.values.length > 0) {
      this.emit(' ');
      if (node.values.length === 1) {
        this.emit(this.visitExpression(node.values[0]));
      } else {
        // Multiple return values become a tuple
        this.emit('[');
        for (let i = 0; i < node.values.length; i++) {
          if (i > 0) this.emit(', ');
          this.emit(this.visitExpression(node.values[i]));
        }
        this.emit(']');
      }
    }
    this.emitLine(';');
  }

  private visitBreak(node: AST.Break) {
    this.emit('break');
    if (node.label) {
      this.emit(' ');
      this.emit(this.visitIdentifier(node.label));
    }
    this.emitLine(';');
  }

  private visitContinue(node: AST.Continue) {
    this.emit('continue');
    if (node.label) {
      this.emit(' ');
      this.emit(this.visitIdentifier(node.label));
    }
    this.emitLine(';');
  }

  private visitThrow(node: AST.Throw) {
    this.emit('throw ');
    this.emit(this.visitExpression(node.value));
    this.emitLine(';');
  }

  private visitBlock(node: AST.Block) {
    this.emitLine('{');
    this.indent();
    
    for (const stmt of node.statements) {
      this.visitNode(stmt);
    }
    
    this.dedent();
    this.emit(this.options.indent.repeat(this.indentLevel) + '}');
  }

  // Expressions

  private visitExpression(node: AST.Expr): string {
    switch (node.kind) {
      case 'Identifier':
        return this.visitIdentifier(node as AST.Identifier);
      case 'NumericLiteral':
        return this.visitNumericLiteral(node as AST.NumericLiteral);
      case 'StringLiteral':
        return this.visitStringLiteral(node as AST.StringLiteral);
      case 'BooleanLiteral':
        return this.visitBooleanLiteral(node as AST.BooleanLiteral);
      case 'NullLiteral':
        return 'null';
      case 'ArrayLiteral':
        return this.visitArrayLiteral(node as AST.ArrayLiteral);
      case 'ObjectLiteral':
        return this.visitObjectLiteral(node as AST.ObjectLiteral);
      case 'Lambda':
        return this.visitLambda(node as AST.Lambda);
      case 'Call':
        return this.visitCall(node as AST.Call);
      case 'Member':
        return this.visitMember(node as AST.Member);
      case 'Index':
        return this.visitIndex(node as AST.Index);
      case 'Unary':
        return this.visitUnary(node as AST.Unary);
      case 'Binary':
        return this.visitBinary(node as AST.Binary);
      case 'Assign':
        return this.visitAssign(node as AST.Assign);
      case 'Ternary':
        return this.visitTernary(node as AST.Ternary);
      case 'Yield':
        return this.visitYield(node as AST.Yield);
      case 'Spread':
        return this.visitSpread(node as AST.Spread);
      case 'TypeAssertion':
        return this.visitTypeAssertion(node as AST.TypeAssertion);
      default:
        return `/* TODO: ${node.kind} */`;
    }
  }

  private visitIdentifier(node: AST.Identifier): string {
    return node.name;
  }

  private visitNumericLiteral(node: AST.NumericLiteral): string {
    return node.raw;
  }

  private visitStringLiteral(node: AST.StringLiteral): string {
    // Check if it has interpolations (template literal)
    const hasInterpolations = node.parts.some(p => p.kind === 'Interpolation');
    
    if (hasInterpolations) {
      // Template literal
      let result = '`';
      for (const part of node.parts) {
        if (part.kind === 'Text') {
          const text = (part.value as string)
            .replace(/\\/g, '\\\\')
            .replace(/`/g, '\\`')
            .replace(/\$/g, '\\$');
          result += text;
        } else {
          // Interpolation
          result += '${';
          result += this.visitExpression(part.value as AST.Expr);
          result += '}';
        }
      }
      result += '`';
      return result;
    } else {
      // Regular string
      const text = node.parts
        .filter(p => p.kind === 'Text')
        .map(p => p.value as string)
        .join('');
      const escaped = text
        .replace(/\\/g, '\\\\')
        .replace(/'/g, "\\'")
        .replace(/\n/g, '\\n')
        .replace(/\r/g, '\\r')
        .replace(/\t/g, '\\t');
      return `'${escaped}'`;
    }
  }

  private visitBooleanLiteral(node: AST.BooleanLiteral): string {
    return node.value ? 'true' : 'false';
  }

  private visitArrayLiteral(node: AST.ArrayLiteral): string {
    let result = '[';
    for (let i = 0; i < node.elements.length; i++) {
      if (i > 0) result += ', ';
      result += this.visitExpression(node.elements[i]);
    }
    result += ']';
    return result;
  }

  private visitObjectLiteral(node: AST.ObjectLiteral): string {
    if (node.properties.length === 0) return '{}';
    
    let result = '{ ';
    for (let i = 0; i < node.properties.length; i++) {
      if (i > 0) result += ', ';
      const prop: any = node.properties[i];
      
      // Check if it's a spread
      if (prop.kind === 'Spread') {
        result += '...';
        result += this.visitExpression(prop.argument);
      } else {
        // It's an ObjectProperty
        const objProp = prop as AST.ObjectProperty;
        
        // Handle computed property names
        if (objProp.computed) {
          result += '[';
          result += this.visitExpression(objProp.key);
          result += ']';
        } else if (objProp.key.kind === 'Identifier') {
          result += this.visitIdentifier(objProp.key as AST.Identifier);
        } else {
          result += this.visitExpression(objProp.key);
        }
        
        if (objProp.shorthand && objProp.key.kind === 'Identifier') {
          // Shorthand property
        } else {
          result += ': ';
          result += this.visitExpression(objProp.value);
        }
      }
    }
    result += ' }';
    return result;
  }

  private visitLambda(node: AST.Lambda): string {
    // Lambda expressions become arrow functions
    let result = '';
    if (node.async) result += 'async ';
    
    // Simple parameter case
    if (node.params.length === 1 && !node.params[0].type && !node.params[0].defaultValue) {
      const param = node.params[0];
      if (param.name.kind === 'Identifier') {
        result += this.visitIdentifier(param.name);
      } else {
        // For destructuring patterns, use parentheses
        result += '(' + this.visitParam(param, true) + ')';
      }
    } else {
      result += '(';
      for (let i = 0; i < node.params.length; i++) {
        if (i > 0) result += ', ';
        result += this.visitParam(node.params[i], true) as string;
      }
      result += ')';
    }
    
    if (node.returnType) {
      result += ': ' + this.visitType(node.returnType);
    }
    
    result += ' => ';
    
    if (node.body.kind === 'Block') {
      // Convert block to inline
      const block = node.body as AST.Block;
      result += '{ ';
      // Simple representation for now
      result += '/* body */ ';
      result += '}';
    } else {
      result += this.visitExpression(node.body as AST.Expr);
    }
    
    return result;
  }

  private visitCall(node: AST.Call): string {
    let result = this.visitExpression(node.callee);
    result += '(';
    for (let i = 0; i < node.args.length; i++) {
      if (i > 0) result += ', ';
      result += this.visitExpression(node.args[i]);
    }
    result += ')';
    return result;
  }

  private visitMember(node: AST.Member): string {
    let result = this.visitExpression(node.object);
    result += node.optional ? '?.' : '.';
    result += this.visitIdentifier(node.property);
    return result;
  }

  private visitIndex(node: AST.Index): string {
    let result = this.visitExpression(node.object);
    result += node.optional ? '?.[' : '[';
    result += this.visitExpression(node.index);
    result += ']';
    return result;
  }

  private visitUnary(node: AST.Unary): string {
    if (node.prefix) {
      return node.op + this.visitExpression(node.argument);
    } else {
      return this.visitExpression(node.argument) + node.op;
    }
  }

  private visitBinary(node: AST.Binary): string {
    const left = this.visitExpression(node.left);
    const right = this.visitExpression(node.right);
    
    // Convert PolyScript-specific operators to TypeScript
    let op = node.op;
    if (op === 'and') op = '&&';
    if (op === 'or') op = '||';
    if (op === 'not') op = '!';
    
    return `${left} ${op} ${right}`;
  }

  private visitAssign(node: AST.Assign): string {
    const left = this.visitExpression(node.left);
    const right = this.visitExpression(node.right);
    return `${left} ${node.op} ${right}`;
  }

  private visitTernary(node: AST.Ternary): string {
    const test = this.visitExpression(node.test);
    const consequent = this.visitExpression(node.consequent);
    const alternate = this.visitExpression(node.alternate);
    return `${test} ? ${consequent} : ${alternate}`;
  }

  private visitYield(node: AST.Yield): string {
    let result = 'yield';
    if (node.delegate) result += '*';
    if (node.value) {
      result += ' ' + this.visitExpression(node.value);
    }
    return result;
  }

  private visitSpread(node: AST.Spread): string {
    return '...' + this.visitExpression(node.argument);
  }

  private visitTypeAssertion(node: AST.TypeAssertion): string {
    return `${this.visitExpression(node.expr)} as ${this.visitType(node.type)}`;
  }

  // Helper methods

  private visitParam(param: AST.Param, inline = false): string | void {
    let nameStr: string;
    
    // Handle destructuring patterns
    if (param.name.kind === 'ArrayPattern') {
      nameStr = this.visitArrayPattern(param.name);
    } else if (param.name.kind === 'ObjectPattern') {
      nameStr = this.visitObjectPattern(param.name);
    } else {
      nameStr = this.visitIdentifier(param.name);
    }
    
    const result = nameStr + 
      (param.type ? ': ' + this.visitType(param.type) : '') +
      (param.defaultValue ? ' = ' + this.visitExpression(param.defaultValue) : '');
    
    if (inline) return result;
    this.emit(result);
  }
  
  private visitArrayPattern(pattern: AST.ArrayPattern): string {
    let result = '[';
    for (let i = 0; i < pattern.elements.length; i++) {
      if (i > 0) result += ', ';
      const elem = pattern.elements[i];
      if (elem === null) {
        // Hole in array pattern
        continue;
      } else if (elem.kind === 'Identifier') {
        result += this.visitIdentifier(elem);
      } else if (elem.kind === 'ArrayPattern') {
        result += this.visitArrayPattern(elem);
      } else if (elem.kind === 'ObjectPattern') {
        result += this.visitObjectPattern(elem);
      }
    }
    result += ']';
    return result;
  }
  
  private visitObjectPattern(pattern: AST.ObjectPattern): string {
    let result = '{';
    for (let i = 0; i < pattern.properties.length; i++) {
      if (i > 0) result += ', ';
      const prop = pattern.properties[i];
      result += this.visitIdentifier(prop.key);
      if (!prop.shorthand) {
        result += ': ';
        if (prop.value.kind === 'Identifier') {
          result += this.visitIdentifier(prop.value);
        } else if (prop.value.kind === 'ArrayPattern') {
          result += this.visitArrayPattern(prop.value);
        } else if (prop.value.kind === 'ObjectPattern') {
          result += this.visitObjectPattern(prop.value);
        }
      }
    }
    result += '}';
    return result;
  }

  private visitType(type: AST.TypeNode): string {
    switch (type.kind) {
      case 'SimpleType':
        return this.visitIdentifier(type.id);
      case 'GenericType':
        let result = this.visitIdentifier(type.base) + '<';
        for (let i = 0; i < type.args.length; i++) {
          if (i > 0) result += ', ';
          result += this.visitType(type.args[i]);
        }
        result += '>';
        return result;
      case 'UnionType':
        return type.types.map(t => this.visitType(t)).join(' | ');
      case 'NullableType':
        return this.visitType(type.inner) + ' | null';
      case 'FuncType':
        let fnType = '(';
        for (let i = 0; i < type.params.length; i++) {
          if (i > 0) fnType += ', ';
          const param = type.params[i];
          // Use parameter name if available, otherwise generate one
          const paramName = param.name ? this.visitIdentifier(param.name) : `arg${i}`;
          fnType += paramName;
          if (param.optional) fnType += '?';
          fnType += ': ' + this.visitType(param.type);
        }
        fnType += ') => ' + this.visitType(type.ret);
        return fnType;
      case 'ChanType':
        // TypeScript doesn't have channels, use a custom type
        return type.elementType ? `Channel<${this.visitType(type.elementType)}>` : 'Channel<any>';
      case 'ImplType':
        // Impl types become the concrete type
        return this.visitType(type.trait);
      default:
        return 'any';
    }
  }

  private visitClassMember(member: AST.ClassMember) {
    if (member.static) this.emit('static ');
    if (member.readonly) this.emit('readonly ');
    if (member.visibility === 'private') this.emit('private ');
    if (member.visibility === 'protected') this.emit('protected ');
    if (member.visibility === 'public') this.emit('public ');
    
    if (member.kind === 'Constructor') {
      this.emit('constructor(');
      if (member.params) {
        for (let i = 0; i < member.params.length; i++) {
          if (i > 0) this.emit(', ');
          this.visitParam(member.params[i]);
        }
      }
      this.emit(') ');
      if (member.body) {
        this.visitBlock(member.body);
      } else {
        this.emitLine('{}');
      }
    } else if (member.kind === 'Method') {
      if (member.name) {
        this.emit(this.visitIdentifier(member.name));
      }
      this.emit('(');
      if (member.params) {
        for (let i = 0; i < member.params.length; i++) {
          if (i > 0) this.emit(', ');
          this.visitParam(member.params[i]);
        }
      }
      this.emit(')');
      if (member.type) {
        this.emit(': ');
        this.emit(this.visitType(member.type));
      }
      this.emit(' ');
      if (member.body) {
        this.visitBlock(member.body);
      } else {
        this.emitLine('{}');
      }
    } else if (member.kind === 'Field') {
      if (member.name) {
        this.emit(this.visitIdentifier(member.name));
      }
      if (member.type) {
        this.emit(': ');
        this.emit(this.visitType(member.type));
      }
      this.emitLine(';');
    }
  }

  private visitInterfaceMember(member: AST.InterfaceMember) {
    this.emit(this.visitIdentifier(member.name));
    
    // Handle method signatures
    if (member.kind === "Method") {
      // Add generic parameters if present
      if (member.genericParams && member.genericParams.length > 0) {
        this.emit('<');
        member.genericParams.forEach((param, i) => {
          if (i > 0) this.emit(', ');
          this.emit(this.visitIdentifier(param));
        });
        this.emit('>');
      }
      
      // Add parameters
      this.emit('(');
      if (member.params) {
        member.params.forEach((param, i) => {
          if (i > 0) this.emit(', ');
          // Handle destructuring patterns in interface methods
          if (param.name.kind === 'Identifier') {
            this.emit(this.visitIdentifier(param.name));
          } else if (param.name.kind === 'ArrayPattern') {
            this.emit(this.visitArrayPattern(param.name));
          } else if (param.name.kind === 'ObjectPattern') {
            this.emit(this.visitObjectPattern(param.name));
          }
          if (param.type) {
            this.emit(': ');
            this.emit(this.visitType(param.type));
          }
        });
      }
      this.emit(')');
      
      // Add return type
      if (member.returnType) {
        this.emit(': ');
        this.emit(this.visitType(member.returnType));
      }
    } else {
      // Handle property signatures
      if (member.optional) this.emit('?');
      if (member.type) {
        this.emit(': ');
        this.emit(this.visitType(member.type));
      }
    }
    
    this.emitLine(';');
  }
}