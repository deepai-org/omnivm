#!/usr/bin/env node

const readline = require('readline');
const util = require('util');
const { Lexer } = require('./dist/lexer');
const { Parser } = require('./dist/parser');

// ANSI color codes
const colors = {
  reset: '\x1b[0m',
  bright: '\x1b[1m',
  dim: '\x1b[2m',
  red: '\x1b[31m',
  green: '\x1b[32m',
  yellow: '\x1b[33m',
  blue: '\x1b[34m',
  magenta: '\x1b[35m',
  cyan: '\x1b[36m',
  white: '\x1b[37m'
};

// Simple AST to TypeScript transpiler
function astToTypeScript(node, indent = '') {
  if (!node) return '';
  
  switch (node.kind) {
    case 'Program':
      return node.body.map(stmt => astToTypeScript(stmt)).join('\n');
    
    case 'FuncDecl':
      const asyncMod = node.async ? 'async ' : '';
      const params = node.params.map(p => {
        const type = p.type ? `: ${typeToTS(p.type)}` : '';
        return `${p.name.name}${type}`;
      }).join(', ');
      const returnType = node.returnType ? `: ${typeToTS(node.returnType)}` : '';
      const body = astToTypeScript(node.body, indent);
      return `${indent}${asyncMod}function ${node.name.name}(${params})${returnType} ${body}`;
    
    case 'ClassDecl':
      const className = node.name.name;
      const genericParams = node.genericParams ? 
        `<${node.genericParams.map(g => g.name).join(', ')}>` : '';
      const ext = node.extends ? ` extends ${typeToTS(node.extends)}` : '';
      const impl = node.implements && node.implements.length > 0 ? 
        ` implements ${node.implements.map(typeToTS).join(', ')}` : '';
      const members = node.members.map(m => astToTypeScript(m, indent + '  ')).join('\n');
      return `${indent}class ${className}${genericParams}${ext}${impl} {\n${members}\n${indent}}`;
    
    case 'ConstDecl':
      const names = node.names.map(n => n.name).join(', ');
      const values = node.values.map(v => astToTypeScript(v)).join(', ');
      return `${indent}const ${names} = ${values};`;
    
    case 'VarDecl':
      const varNames = node.names.map(n => n.name).join(', ');
      const varValues = node.values.map(v => astToTypeScript(v)).join(', ');
      return `${indent}let ${varNames} = ${varValues};`;
    
    case 'Lambda':
      const lambdaParams = node.params.map(p => p.name.name).join(', ');
      const lambdaBody = node.body.statements ? 
        `{\n${node.body.statements.map(s => astToTypeScript(s, indent + '  ')).join('\n')}\n${indent}}` :
        astToTypeScript(node.body);
      return `(${lambdaParams}) => ${lambdaBody}`;
    
    case 'Block':
      const statements = node.statements.map(s => astToTypeScript(s, indent + '  ')).join('\n');
      return `{\n${statements}\n${indent}}`;
    
    case 'Return':
      const retValues = node.values.map(v => astToTypeScript(v)).join(', ');
      return `${indent}return ${retValues};`;
    
    case 'If':
      const cond = astToTypeScript(node.test);
      const then = astToTypeScript(node.consequent, indent);
      const elsePart = node.alternate ? ` else ${astToTypeScript(node.alternate, indent)}` : '';
      return `${indent}if (${cond}) ${then}${elsePart}`;
    
    case 'Loop':
      if (node.mode === 'for') {
        const init = node.init ? astToTypeScript(node.init) : '';
        const test = node.test ? astToTypeScript(node.test) : '';
        const step = node.step ? astToTypeScript(node.step) : '';
        const body = astToTypeScript(node.body, indent);
        return `${indent}for (${init}; ${test}; ${step}) ${body}`;
      } else if (node.mode === 'foreach') {
        const variable = astToTypeScript(node.variable);
        const iterable = astToTypeScript(node.iterable);
        const body = astToTypeScript(node.body, indent);
        return `${indent}for (const ${variable} of ${iterable}) ${body}`;
      } else if (node.mode === 'while') {
        const test = astToTypeScript(node.test);
        const body = astToTypeScript(node.body, indent);
        return `${indent}while (${test}) ${body}`;
      }
      return `${indent}// Unsupported loop type: ${node.mode}`;
    
    case 'Binary':
      const left = astToTypeScript(node.left);
      const right = astToTypeScript(node.right);
      return `${left} ${node.op} ${right}`;
    
    case 'Unary':
      const operand = astToTypeScript(node.argument);
      return node.prefix ? `${node.op}${operand}` : `${operand}${node.op}`;
    
    case 'Call':
      const callee = astToTypeScript(node.callee);
      const args = node.args.map(astToTypeScript).join(', ');
      return `${callee}(${args})`;
    
    case 'MemberAccess':
      const obj = astToTypeScript(node.object);
      const prop = astToTypeScript(node.property);
      return node.computed ? `${obj}[${prop}]` : `${obj}.${prop}`;
    
    case 'Identifier':
      return node.name;
    
    case 'NumericLiteral':
      return node.raw;
    
    case 'StringLiteral':
      return node.raw;
    
    case 'BooleanLiteral':
      return node.value.toString();
    
    case 'ArrayLiteral':
      const elements = node.elements.map(astToTypeScript).join(', ');
      return `[${elements}]`;
    
    case 'ObjectLiteral':
      const properties = node.properties.map(p => {
        const key = p.key.kind === 'Identifier' ? p.key.name : astToTypeScript(p.key);
        const value = astToTypeScript(p.value);
        return `${key}: ${value}`;
      }).join(', ');
      return `{${properties}}`;
    
    case 'JSXElement':
      const tag = node.openingElement.tagName.name;
      const attrs = node.openingElement.attributes.map(attr => {
        if (attr.kind === 'JSXAttribute') {
          const name = attr.name.name;
          const value = attr.value ? astToTypeScript(attr.value) : 'true';
          return `${name}={${value}}`;
        }
        return `{...${astToTypeScript(attr.argument)}}`;
      }).join(' ');
      const children = node.children.map(astToTypeScript).join('');
      const attrStr = attrs ? ` ${attrs}` : '';
      
      if (node.selfClosing) {
        return `<${tag}${attrStr} />`;
      }
      return `<${tag}${attrStr}>${children}</${tag}>`;
    
    case 'JSXText':
      return node.value;
    
    case 'JSXExpression':
      return `{${astToTypeScript(node.expression)}}`;
    
    case 'JSXFragment':
      const fragChildren = node.children.map(astToTypeScript).join('');
      return `<>${fragChildren}</>`;
    
    case 'ExprStmt':
      return `${indent}${astToTypeScript(node.expr)};`;
    
    case 'ShortDecl':
      const pairs = node.pairs.map(p => `${p.name.name} = ${astToTypeScript(p.expr)}`).join(', ');
      return `${indent}const ${pairs};`;
    
    case 'ImportDecl':
      const specs = node.specifiers.map(s => {
        if (s.kind === 'ImportDefaultSpecifier') return s.local.name;
        if (s.kind === 'ImportNamespaceSpecifier') return `* as ${s.local.name}`;
        if (s.kind === 'ImportSpecifier') {
          return s.imported.name === s.local.name ? 
            s.local.name : 
            `${s.imported.name} as ${s.local.name}`;
        }
        return '';
      }).filter(Boolean).join(', ');
      return `${indent}import ${specs} from ${node.source.raw};`;
    
    case 'ExportDecl':
      if (node.declaration) {
        return `${indent}export ${astToTypeScript(node.declaration, indent)}`;
      }
      if (node.specifiers && node.specifiers.length > 0) {
        const specs = node.specifiers.map(s => {
          return s.exported.name === s.local.name ? 
            s.local.name : 
            `${s.local.name} as ${s.exported.name}`;
        }).join(', ');
        const source = node.source ? ` from ${node.source.raw}` : '';
        return `${indent}export { ${specs} }${source};`;
      }
      return `${indent}export {};`;
    
    case 'Try':
      const tryBlock = astToTypeScript(node.body, indent);
      const catchClause = node.handler ? 
        ` catch ${node.handler.param ? `(${node.handler.param.name})` : ''} ${astToTypeScript(node.handler.body, indent)}` : '';
      const finallyClause = node.finalizer ? 
        ` finally ${astToTypeScript(node.finalizer, indent)}` : '';
      return `${indent}try ${tryBlock}${catchClause}${finallyClause}`;
    
    case 'Throw':
      return `${indent}throw ${astToTypeScript(node.argument)};`;
    
    case 'TypeAlias':
      const typeParams = node.typeParams ? 
        `<${node.typeParams.map(p => p.name).join(', ')}>` : '';
      return `${indent}type ${node.name.name}${typeParams} = ${typeToTS(node.type)};`;
    
    case 'InterfaceDecl':
      const intName = node.name.name;
      const intGenerics = node.typeParams ? 
        `<${node.typeParams.map(p => p.name).join(', ')}>` : '';
      const intExtends = node.extends && node.extends.length > 0 ? 
        ` extends ${node.extends.map(typeToTS).join(', ')}` : '';
      const intMembers = node.members.map(m => {
        if (m.kind === 'Method') {
          const params = m.params.map(p => `${p.name.name}: any`).join(', ');
          return `  ${m.name.name}(${params}): any;`;
        }
        return `  ${m.name.name}: any;`;
      }).join('\n');
      return `${indent}interface ${intName}${intGenerics}${intExtends} {\n${intMembers}\n${indent}}`;
      
    default:
      return `${indent}/* Unsupported AST node: ${node.kind} */`;
  }
}

function typeToTS(type) {
  if (!type) return 'any';
  
  switch (type.kind) {
    case 'SimpleType':
      return type.id.name;
    case 'GenericType':
      const base = type.base.name;
      const args = type.args.map(typeToTS).join(', ');
      return `${base}<${args}>`;
    case 'ArrayType':
      return `${typeToTS(type.elementType)}[]`;
    case 'UnionType':
      return type.types.map(typeToTS).join(' | ');
    case 'IntersectionType':
      return type.types.map(typeToTS).join(' & ');
    case 'FunctionType':
      const params = type.params.map((p, i) => `arg${i}: ${typeToTS(p)}`).join(', ');
      const ret = typeToTS(type.returnType);
      return `(${params}) => ${ret}`;
    default:
      return 'any';
  }
}

// Create readline interface
const rl = readline.createInterface({
  input: process.stdin,
  output: process.stdout,
  prompt: `${colors.cyan}poly> ${colors.reset}`
});

console.log(`${colors.bright}${colors.green}PolyScript REPL${colors.reset}`);
console.log(`${colors.dim}Type code to see AST and TypeScript transpilation${colors.reset}`);
console.log(`${colors.dim}Commands: .exit, .clear, .ast, .ts, .both (default)${colors.reset}`);
console.log();

let mode = 'both'; // 'ast', 'ts', or 'both'
let multilineBuffer = '';
let inMultiline = false;

rl.prompt();

rl.on('line', (line) => {
  // Handle commands
  if (line.trim().startsWith('.')) {
    const command = line.trim();
    switch (command) {
      case '.exit':
        rl.close();
        return;
      case '.clear':
        console.clear();
        console.log(`${colors.bright}${colors.green}PolyScript REPL${colors.reset}`);
        break;
      case '.ast':
        mode = 'ast';
        console.log(`${colors.yellow}Mode: AST only${colors.reset}`);
        break;
      case '.ts':
        mode = 'ts';
        console.log(`${colors.yellow}Mode: TypeScript only${colors.reset}`);
        break;
      case '.both':
        mode = 'both';
        console.log(`${colors.yellow}Mode: AST and TypeScript${colors.reset}`);
        break;
      default:
        console.log(`${colors.red}Unknown command: ${command}${colors.reset}`);
    }
    rl.prompt();
    return;
  }

  // Handle multiline input
  if (line.trim().endsWith('{') || line.trim().endsWith(':')) {
    multilineBuffer += line + '\n';
    inMultiline = true;
    rl.setPrompt(`${colors.dim}...${colors.reset} `);
    rl.prompt();
    return;
  }
  
  if (inMultiline) {
    multilineBuffer += line + '\n';
    // Simple heuristic: if line is empty or starts with dedent, end multiline
    if (line.trim() === '' || (line.length > 0 && line[0] !== ' ' && line[0] !== '\t')) {
      inMultiline = false;
      line = multilineBuffer.trim();
      multilineBuffer = '';
      rl.setPrompt(`${colors.cyan}poly> ${colors.reset}`);
    } else {
      rl.prompt();
      return;
    }
  }

  if (!line.trim()) {
    rl.prompt();
    return;
  }

  try {
    // Parse the code
    const lexer = new Lexer(line);
    const tokens = lexer.tokenize();
    const parser = new Parser(tokens);
    const ast = parser.parse();

    if (mode === 'ast' || mode === 'both') {
      console.log(`\n${colors.magenta}=== AST ===${colors.reset}`);
      console.log(util.inspect(ast, { 
        colors: true, 
        depth: null, 
        compact: false 
      }));
    }

    if (mode === 'ts' || mode === 'both') {
      console.log(`\n${colors.blue}=== TypeScript ===${colors.reset}`);
      const tsCode = astToTypeScript(ast);
      console.log(tsCode || `${colors.dim}(empty output)${colors.reset}`);
    }

    console.log();
  } catch (error) {
    console.log(`${colors.red}Error: ${error.message}${colors.reset}`);
    if (error.stack && process.env.DEBUG) {
      console.log(`${colors.dim}${error.stack}${colors.reset}`);
    }
  }

  rl.prompt();
});

rl.on('close', () => {
  console.log(`\n${colors.green}Goodbye!${colors.reset}`);
  process.exit(0);
});

// Handle Ctrl+C gracefully
process.on('SIGINT', () => {
  if (inMultiline) {
    inMultiline = false;
    multilineBuffer = '';
    rl.setPrompt(`${colors.cyan}poly> ${colors.reset}`);
    console.log(`${colors.yellow}(multiline input cancelled)${colors.reset}`);
    rl.prompt();
  } else {
    rl.close();
  }
});