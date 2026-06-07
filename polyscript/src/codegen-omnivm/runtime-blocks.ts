import * as AST from '../ast';
import { OmniRuntime, RuntimeAffinity } from '../runtime-resolver/types';

/**
 * Represents a contiguous block of AST nodes that share the same runtime.
 */
export interface RuntimeBlock {
  runtime: OmniRuntime;
  nodes: (AST.Decl | AST.Stmt | AST.Expr)[];
}

/**
 * Consolidate adjacent same-runtime nodes into blocks.
 *
 * When multiple consecutive nodes resolve to the same non-JS runtime,
 * they can be emitted as a single `omnivm.call()` instead of one per node.
 * This reduces bridge overhead significantly.
 *
 * JS nodes are left as individual entries since they run natively.
 */
export function consolidateBlocks(
  nodes: (AST.Decl | AST.Stmt)[],
  affinityMap: Map<AST.Decl | AST.Stmt | AST.Expr, RuntimeAffinity>,
): RuntimeBlock[] {
  const blocks: RuntimeBlock[] = [];
  let currentBlock: RuntimeBlock | null = null;

  for (const node of nodes) {
    const affinity = affinityMap.get(node);
    const runtime = affinity?.runtime || OmniRuntime.JavaScript;

    if (currentBlock && currentBlock.runtime === runtime && runtime !== OmniRuntime.JavaScript) {
      // Same non-JS runtime as current block — consolidate
      currentBlock.nodes.push(node);
    } else {
      // Different runtime or JS — start a new block
      if (currentBlock) {
        blocks.push(currentBlock);
      }
      currentBlock = { runtime, nodes: [node] };
    }
  }

  if (currentBlock) {
    blocks.push(currentBlock);
  }

  return blocks;
}

/**
 * Check if a block is a single-runtime block that can be consolidated
 * into one omnivm.call().
 */
export function isConsolidatable(block: RuntimeBlock): boolean {
  return block.runtime !== OmniRuntime.JavaScript && block.nodes.length > 1;
}

/**
 * Check if a runtime requires compilation rather than interpretation.
 * C and Rust nodes are compiled to shared libraries via gcc/rustc.
 */
export function isCompiledRuntime(runtime: OmniRuntime): boolean {
  return runtime === OmniRuntime.Rust || runtime === OmniRuntime.C;
}
