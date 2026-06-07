import { Lexer } from '../src/lexer';
import { Parser } from '../src/parser';
import * as AST from '../src/ast';
import { parseCode } from './helpers';

describe('Parser - Deep Generic Nesting', () => {
  
  describe('Token splitting at various depths', () => {
    
    test('parses 4-level nested generics with >>>', () => {
      const code = `let x: Map<string, Array<Result<Option<T>>>>`;
      const ast = parseCode(code);
      
      const varDecl = ast.body[0] as AST.VarDecl;
      expect(varDecl.kind).toBe('VarDecl');
      
      const type = varDecl.type as AST.GenericType;
      expect(type.kind).toBe('GenericType');
      expect(type.base.name).toBe('Map');
      expect(type.args).toHaveLength(2);
      
      // Second arg: Array<Result<Option<T>>>
      const arrayType = type.args[1] as AST.GenericType;
      expect(arrayType.kind).toBe('GenericType');
      expect(arrayType.base.name).toBe('Array');
      
      // Inside Array: Result<Option<T>>
      const resultType = arrayType.args[0] as AST.GenericType;
      expect(resultType.kind).toBe('GenericType');
      expect(resultType.base.name).toBe('Result');
      
      // Inside Result: Option<T>
      const optionType = resultType.args[0] as AST.GenericType;
      expect(optionType.kind).toBe('GenericType');
      expect(optionType.base.name).toBe('Option');
      
      // Inside Option: T
      const tType = optionType.args[0] as AST.SimpleType;
      expect(tType.kind).toBe('SimpleType');
      expect(tType.id.name).toBe('T');
    });
    
    test('parses 5-level nested generics with multiple >>>', () => {
      const code = `type Deep = Box<Vec<Map<string, List<Option<int>>>>>`;
      const ast = parseCode(code);
      
      const typeDecl = ast.body[0] as AST.TypeDecl;
      expect(typeDecl.kind).toBe('TypeDecl');
      
      const type = typeDecl.definition as AST.GenericType;
      expect(type.kind).toBe('GenericType');
      expect(type.base.name).toBe('Box');
      
      // Drill down 5 levels
      const vec = type.args[0] as AST.GenericType;
      expect(vec.base.name).toBe('Vec');
      
      const map = vec.args[0] as AST.GenericType;
      expect(map.base.name).toBe('Map');
      expect(map.args).toHaveLength(2);
      
      const list = map.args[1] as AST.GenericType;
      expect(list.base.name).toBe('List');
      
      const option = list.args[0] as AST.GenericType;
      expect(option.base.name).toBe('Option');
      
      const int = option.args[0] as AST.SimpleType;
      expect(int.id.name).toBe('int');
    });
    
    test('parses 6-level nested generics', () => {
      const code = `let f: Fn<A, Result<B, Vec<HashMap<string, Option<Box<dyn Error>>>>>>`;
      const ast = parseCode(code);
      
      const varDecl = ast.body[0] as AST.VarDecl;
      const fnType = varDecl.type as AST.GenericType;
      expect(fnType.base.name).toBe('Fn');
      expect(fnType.args).toHaveLength(2);
      
      // Second arg has 5 levels of nesting
      const result = fnType.args[1] as AST.GenericType;
      expect(result.base.name).toBe('Result');
      
      const vec = result.args[1] as AST.GenericType;
      expect(vec.base.name).toBe('Vec');
      
      const hashMap = vec.args[0] as AST.GenericType;
      expect(hashMap.base.name).toBe('HashMap');
      
      const option = hashMap.args[1] as AST.GenericType;
      expect(option.base.name).toBe('Option');
      
      const box = option.args[0] as AST.GenericType;
      expect(box.base.name).toBe('Box');
      
      // dyn Error becomes a DynType
      const dynError = box.args[0] as AST.DynType;
      expect(dynError.kind).toBe('DynType');
    });
    
    test('parses 7-level channel nesting with >>>', () => {
      const code = `let c: chan<chan<chan<chan<chan<chan<chan<int>>>>>>>`;
      const ast = parseCode(code);
      
      const varDecl = ast.body[0] as AST.VarDecl;
      let current = varDecl.type as AST.GenericType;
      
      // Verify 7 levels of chan nesting
      for (let i = 0; i < 7; i++) {
        expect(current.kind).toBe('GenericType');
        expect(current.base.name).toBe('chan');
        expect(current.args).toHaveLength(1);
        
        if (i < 6) {
          current = current.args[0] as AST.GenericType;
        } else {
          // Last level should be int
          const innermost = current.args[0] as AST.SimpleType;
          expect(innermost.kind).toBe('SimpleType');
          expect(innermost.id.name).toBe('int');
        }
      }
    });
    
    test('parses mixed >> and >>> at different levels', () => {
      // Fixed: Need >>> after G to close F, E, D, then H<I> is arg to A
      const code = `type X = A<B<C>, D<E<F<G>>>, H<I>>`;
      const ast = parseCode(code);
      
      const typeDecl = ast.body[0] as AST.TypeDecl;
      const type = typeDecl.definition as AST.GenericType;
      
      expect(type.base.name).toBe('A');
      expect(type.args).toHaveLength(3);
      
      // First arg: B<C>
      const b = type.args[0] as AST.GenericType;
      expect(b.base.name).toBe('B');
      expect((b.args[0] as AST.SimpleType).id.name).toBe('C');
      
      // Second arg: D<E<F<G>>>
      const d = type.args[1] as AST.GenericType;
      expect(d.base.name).toBe('D');
      const e = d.args[0] as AST.GenericType;
      expect(e.base.name).toBe('E');
      const f = e.args[0] as AST.GenericType;
      expect(f.base.name).toBe('F');
      expect((f.args[0] as AST.SimpleType).id.name).toBe('G');
      
      // Third arg: H<I>
      const h = type.args[2] as AST.GenericType;
      expect(h.base.name).toBe('H');
      expect((h.args[0] as AST.SimpleType).id.name).toBe('I');
    });
    
    test('parses function call with 4-level nested generics', () => {
      const code = `let r = process<Vec<Option<Result<T, Error>>>>()`;
      const ast = parseCode(code);
      
      const varDecl = ast.body[0] as AST.VarDecl;
      const call = varDecl.values![0] as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.typeArgs).toHaveLength(1);
      
      const vec = call.typeArgs![0] as AST.GenericType;
      expect(vec.base.name).toBe('Vec');
      
      const option = vec.args[0] as AST.GenericType;
      expect(option.base.name).toBe('Option');
      
      const result = option.args[0] as AST.GenericType;
      expect(result.base.name).toBe('Result');
      expect(result.args).toHaveLength(2);
      expect((result.args[0] as AST.SimpleType).id.name).toBe('T');
      expect((result.args[1] as AST.SimpleType).id.name).toBe('Error');
    });
    
    test('parses 8-level nesting with multiple type parameters', () => {
      // Fixed: Added >> after O to properly close both C and B
      const code = `type Monster = A<B<C<D<E<F<G<H<I, J>, K>, L>, M>, N>, O>>, P, Q>`;
      const ast = parseCode(code);
      
      const typeDecl = ast.body[0] as AST.TypeDecl;
      const a = typeDecl.definition as AST.GenericType;
      
      expect(a.base.name).toBe('A');
      expect(a.args).toHaveLength(3); // B<...>, P, Q
      
      // Navigate down to H which has I, J
      const b = a.args[0] as AST.GenericType;
      expect(b.base.name).toBe('B');
      
      const c = b.args[0] as AST.GenericType;
      expect(c.base.name).toBe('C');
      
      const d = c.args[0] as AST.GenericType;
      expect(d.base.name).toBe('D');
      
      const e = d.args[0] as AST.GenericType;
      expect(e.base.name).toBe('E');
      
      const f = e.args[0] as AST.GenericType;
      expect(f.base.name).toBe('F');
      
      const g = f.args[0] as AST.GenericType;
      expect(g.base.name).toBe('G');
      
      const h = g.args[0] as AST.GenericType;
      expect(h.base.name).toBe('H');
      expect(h.args).toHaveLength(2);
      expect((h.args[0] as AST.SimpleType).id.name).toBe('I');
      expect((h.args[1] as AST.SimpleType).id.name).toBe('J');
      
      // Check other parameters at various levels
      expect((g.args[1] as AST.SimpleType).id.name).toBe('K');
      expect((f.args[1] as AST.SimpleType).id.name).toBe('L');
      expect((e.args[1] as AST.SimpleType).id.name).toBe('M');
      expect((d.args[1] as AST.SimpleType).id.name).toBe('N');
      expect((c.args[1] as AST.SimpleType).id.name).toBe('O');
      expect((a.args[1] as AST.SimpleType).id.name).toBe('P');
      expect((a.args[2] as AST.SimpleType).id.name).toBe('Q');
    });
    
    test('parses maximum >>> splitting (10 levels)', () => {
      // This tests the most extreme case - 10 closing brackets becoming >>>>>> 
      const code = `type Ten = L1<L2<L3<L4<L5<L6<L7<L8<L9<L10<Base>>>>>>>>>>`;
      const ast = parseCode(code);
      
      const typeDecl = ast.body[0] as AST.TypeDecl;
      let current = typeDecl.definition as AST.GenericType;
      
      // Verify all 10 levels
      const expectedNames = ['L1', 'L2', 'L3', 'L4', 'L5', 'L6', 'L7', 'L8', 'L9', 'L10'];
      
      for (const expectedName of expectedNames) {
        expect(current.kind).toBe('GenericType');
        expect(current.base.name).toBe(expectedName);
        expect(current.args).toHaveLength(1);
        current = current.args[0] as AST.GenericType;
      }
      
      // Final level should be Base
      expect((current as unknown as AST.SimpleType).kind).toBe('SimpleType');
      expect((current as unknown as AST.SimpleType).id.name).toBe('Base');
    });
    
    test('handles spaces to avoid >> and >>>', () => {
      // With explicit spaces, no token splitting needed
      const code1 = `type Spaced = A<B<C> >`;
      const code2 = `type Spaced2 = A<B<C<D> > >`;
      
      const ast1 = parseCode(code1);
      const ast2 = parseCode(code2);
      
      // Both should parse correctly
      const type1 = (ast1.body[0] as AST.TypeDecl).definition as AST.GenericType;
      expect(type1.base.name).toBe('A');
      
      const type2 = (ast2.body[0] as AST.TypeDecl).definition as AST.GenericType;
      expect(type2.base.name).toBe('A');
    });
    
    test('stress test: parses 15-level nesting', () => {
      // This is probably beyond practical use but tests the robustness
      const code = `type Stress = T<T<T<T<T<T<T<T<T<T<T<T<T<T<T<X>>>>>>>>>>>>>>>`;
      const ast = parseCode(code);
      
      const typeDecl = ast.body[0] as AST.TypeDecl;
      let current = typeDecl.definition as AST.GenericType;
      
      // Count the depth
      let depth = 0;
      while (current.kind === 'GenericType' && current.base.name === 'T') {
        depth++;
        if (current.args[0]) {
          const next = current.args[0];
          if (next.kind === 'GenericType') {
            current = next;
          } else {
            break;
          }
        }
      }
      
      expect(depth).toBe(15);
      
      // Verify the innermost type
      expect(current.args[0].kind).toBe('SimpleType');
      expect((current.args[0] as AST.SimpleType).id.name).toBe('X');
    });
  });
  
  describe('Edge cases', () => {
    
    test('handles mixed generics and shift operators', () => {
      const code = `let x = foo<A<B<C>>>() >> 2`;
      const ast = parseCode(code);
      
      const varDecl = ast.body[0] as AST.VarDecl;
      const shift = varDecl.values![0] as AST.Binary;
      expect(shift.op).toBe('>>');
      
      const call = shift.left as AST.Call;
      expect(call.kind).toBe('Call');
      expect(call.typeArgs).toHaveLength(1);
      
      // Verify the nested generic was parsed correctly
      const a = call.typeArgs![0] as AST.GenericType;
      expect(a.base.name).toBe('A');
      const b = a.args[0] as AST.GenericType;
      expect(b.base.name).toBe('B');
      const c = b.args[0] as AST.SimpleType;
      expect(c.id.name).toBe('C');
    });
    
    test('handles >>> in function return types', () => {
      const code = `function deep<T>(): Promise<Array<Option<T>>> { }`;
      const ast = parseCode(code);
      
      const func = ast.body[0] as AST.FuncDecl;
      expect(func.genericParams).toHaveLength(1);
      
      const returnType = func.returnType as AST.GenericType;
      expect(returnType.base.name).toBe('Promise');
      
      const array = returnType.args[0] as AST.GenericType;
      expect(array.base.name).toBe('Array');
      
      const option = array.args[0] as AST.GenericType;
      expect(option.base.name).toBe('Option');
      expect((option.args[0] as AST.SimpleType).id.name).toBe('T');
    });
  });
});