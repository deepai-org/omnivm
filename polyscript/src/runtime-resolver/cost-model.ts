import { MarshalKind, OmniRuntime, RuntimeAffinity, BridgeDescriptor } from './types';

/**
 * Bridge crossing costs.
 *
 * | Crossing type            | Cost | Notes                           |
 * |--------------------------|------|---------------------------------|
 * | Primitive                | 1    | Near-zero, copy by value        |
 * | Array of primitives      | 10   | Length-proportional copy         |
 * | Object/dict              | 10   | Key-count proportional          |
 * | Callback/closure         | 100  | Each invocation = bridge call   |
 * | Async bridge             | 200  | Event loop coordination         |
 * | Unknown                  | 50   | Conservative estimate           |
 */
const MARSHAL_COSTS: Record<MarshalKind, number> = {
  [MarshalKind.Primitive]: 1,
  [MarshalKind.Array]: 10,
  [MarshalKind.Object]: 10,
  [MarshalKind.Callback]: 100,
  [MarshalKind.AsyncBridge]: 200,
  [MarshalKind.Unknown]: 50,
};

/**
 * Compute the cost of a bridge crossing.
 */
export function computeBridgeCost(marshalKind: MarshalKind): number {
  return MARSHAL_COSTS[marshalKind];
}

/**
 * Compute the total cost of all bridges in a program.
 */
export function totalBridgeCost(bridges: BridgeDescriptor[]): number {
  return bridges.reduce((sum, b) => sum + b.cost, 0);
}

/**
 * Given a block of nodes with their affinities, determine if it's
 * worth routing ambiguous nodes to the majority runtime to reduce bridges.
 *
 * Returns the recommended runtime if >90% of nodes share one runtime,
 * otherwise returns undefined.
 */
export function majorityRuntime(
  affinities: RuntimeAffinity[],
  threshold = 0.9,
): OmniRuntime | undefined {
  if (affinities.length === 0) return undefined;

  const counts = new Map<OmniRuntime, number>();
  for (const aff of affinities) {
    counts.set(aff.runtime, (counts.get(aff.runtime) || 0) + 1);
  }

  for (const [runtime, count] of counts) {
    if (count / affinities.length >= threshold) {
      return runtime;
    }
  }

  return undefined;
}

/**
 * Optimize bridge placements by rerouting ambiguous (fallback-confidence)
 * nodes to the majority runtime in their enclosing scope.
 *
 * Mutates the affinity map in place and returns the number of
 * rerouted nodes.
 */
export function optimizeBridges(
  affinityMap: Map<any, RuntimeAffinity>,
  scopeNodes: any[],
): number {
  const affinities = scopeNodes
    .map(n => affinityMap.get(n))
    .filter((a): a is RuntimeAffinity => a !== undefined);

  const majority = majorityRuntime(affinities);
  if (!majority) return 0;

  let rerouted = 0;
  for (const node of scopeNodes) {
    const aff = affinityMap.get(node);
    if (aff && aff.confidence === "fallback" && aff.runtime !== majority) {
      aff.runtime = majority;
      aff.evidence.push({
        type: "scope",
        detail: `rerouted to scope majority: ${majority}`,
      });
      rerouted++;
    }
  }

  return rerouted;
}
