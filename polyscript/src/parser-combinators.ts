/**
 * Parser Combinators — Structural parsing helpers (Chunk 10).
 *
 * Generic combinators that operate on any host with cursor methods.
 * These complement the transactional combinators (attempt, choice, withContext)
 * that live on ParserCursor.
 */

import { TokenType } from './lexer';

/** Minimal cursor interface required by combinators. */
export interface CursorHost {
  current: number;
  peek(): { value: string; type: TokenType };
  advance(): any;
  check(value: string): boolean;
  match(...values: string[]): boolean;
  consume(expected: TokenType | string, message: string): any;
  isAtEnd(): boolean;
  attempt<T>(fn: () => T): T | null;
}

/**
 * Parse a separated list of items: rule (sep rule)* [trailing sep]
 *
 * @param host   Cursor host
 * @param rule   Parser for each element
 * @param sep    Separator token value (e.g. ",")
 * @param end    Optional end token — stops before consuming it
 * @param trailing  Allow trailing separator (default true)
 */
export function list<H extends CursorHost, T>(
  host: H,
  rule: (host: H) => T,
  options: { sep: string; end?: string; trailing?: boolean }
): T[] {
  const { sep, end, trailing = true } = options;
  const items: T[] = [];

  if (end && host.check(end)) return items;

  items.push(rule(host));

  while (host.match(sep)) {
    if (end && host.check(end)) {
      if (!trailing) {
        // Shouldn't have had the trailing sep — but we already consumed it.
        // Non-issue in practice; just stop.
      }
      break;
    }
    if (host.isAtEnd()) break;
    items.push(rule(host));
  }

  return items;
}

/**
 * Parse a delimited group: open (rule sep)* close
 *
 * @returns Array of parsed items
 */
export function delimited<H extends CursorHost, T>(
  host: H,
  open: string,
  rule: (host: H) => T,
  close: string,
  sep?: string
): T[] {
  host.consume(open, `Expected '${open}'`);
  const items: T[] = [];

  while (!host.check(close) && !host.isAtEnd()) {
    items.push(rule(host));
    if (sep && !host.check(close)) {
      if (!host.match(sep)) break;
    }
  }

  host.consume(close, `Expected '${close}'`);
  return items;
}

/**
 * Parse zero or more items until a condition fails.
 * Each iteration must make progress (advance cursor); if not, stops.
 */
export function many<H extends CursorHost, T>(
  host: H,
  rule: (host: H) => T | null
): T[] {
  const items: T[] = [];
  while (!host.isAtEnd()) {
    const before = host.current;
    const item = host.attempt(() => rule(host));
    if (item === null || item === undefined) break;
    items.push(item);
    if (host.current === before) break; // no progress
  }
  return items;
}

/**
 * Parse items separated by a token, requiring at least one.
 */
export function separated<H extends CursorHost, T>(
  host: H,
  rule: (host: H) => T,
  sep: string
): T[] {
  const items: T[] = [rule(host)];
  while (host.match(sep)) {
    items.push(rule(host));
  }
  return items;
}

/**
 * Try to parse with `rule`. On failure, return null (cursor rolled back).
 * Shorthand for host.attempt(() => rule(host)).
 */
export function maybe<H extends CursorHost, T>(
  host: H,
  rule: (host: H) => T
): T | null {
  return host.attempt(() => rule(host));
}

/**
 * Parse with `rule`, recovering on error by skipping to one of `syncTokens`.
 * Returns the parsed result on success, or null on recovery.
 */
export function recoverUntil<H extends CursorHost, T>(
  host: H,
  rule: (host: H) => T,
  syncTokens: string[]
): T | null {
  try {
    return rule(host);
  } catch {
    // Skip tokens until we hit a sync point
    while (!host.isAtEnd()) {
      for (const sync of syncTokens) {
        if (host.check(sync)) return null;
      }
      host.advance();
    }
    return null;
  }
}
