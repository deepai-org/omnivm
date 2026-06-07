/**
 * Operator Trie — Longest-match operator scanning.
 *
 * Replaces the linear scan of a sorted operator array with a trie
 * that finds the longest matching operator in O(max_op_length) time.
 */

interface TrieNode {
  children: Map<string, TrieNode>;
  isEnd: boolean;
}

export class OperatorTrie {
  private root: TrieNode = { children: new Map(), isEnd: false };

  constructor(operators: string[]) {
    for (const op of operators) {
      this.insert(op);
    }
  }

  private insert(op: string): void {
    let node = this.root;
    for (const ch of op) {
      if (!node.children.has(ch)) {
        node.children.set(ch, { children: new Map(), isEnd: false });
      }
      node = node.children.get(ch)!;
    }
    node.isEnd = true;
  }

  /**
   * Find the longest operator starting at `pos` in `source`.
   * Returns the operator string, or the single character at `pos` if no multi-char match.
   */
  longestMatch(source: string, pos: number): string {
    let node = this.root;
    let lastMatch = '';
    let i = pos;

    while (i < source.length) {
      const ch = source[i];
      const next = node.children.get(ch);
      if (!next) break;
      node = next;
      i++;
      if (node.isEnd) {
        lastMatch = source.slice(pos, i);
      }
    }

    return lastMatch || source[pos];
  }
}

/** All multi-character operators, sorted longest-first for reference. */
const OPERATORS = [
  '>>>=', '>>>', '>>=', '<<=', '===', '!==', '??=', '**=', '||=', '&&=',
  '<=>', '...', '..', '=>', '==', '!=', '<=', '>=', '<<', '>>', '&&', '||',
  '??', ';;', '|>', '**', '+=', '-=', '*=', '/=', '%=', '&=', '|=', '^=',
  '~=', '=~', '++', '--', ':=:', ':=', '->', '<-', '::', '?.', '!.', '.*'
];

/** Singleton trie built from the operator table. */
export const OPERATOR_TRIE = new OperatorTrie(OPERATORS);
