import { OmniRuntime, RuntimeAffinity } from './types';

export type EvidenceSource =
  | "runtime_tag"
  | "directive"
  | "import"
  | "builtin"
  | "global"
  | "syntax"
  | "keyword"
  | "method"
  | "scope"
  | "fallback";

export interface RuntimeEvidenceFact {
  runtime: OmniRuntime;
  source: EvidenceSource;
  weight: number;
  detail: string;
}

export interface RuntimeEvidenceDecision {
  runtime: OmniRuntime;
  confidence: RuntimeAffinity["confidence"];
  facts: RuntimeEvidenceFact[];
  conflicts: RuntimeEvidenceFact[];
  trace: string[];
}

export const EVIDENCE_WEIGHTS = {
  explicit: Number.POSITIVE_INFINITY,
  directive: 1000,
  import: 800,
  builtin: 750,
  global: 700,
  syntax: 650,
  keyword: 600,
  method: 250,
  scope: 100,
  fallback: 1,
} as const;

export class RuntimeEvidenceGraph<Node extends object> {
  private facts = new Map<Node, RuntimeEvidenceFact[]>();

  add(node: Node, fact: RuntimeEvidenceFact): void {
    const existing = this.facts.get(node) || [];
    existing.push(fact);
    this.facts.set(node, existing);
  }

  get(node: Node): RuntimeEvidenceFact[] {
    return this.facts.get(node) || [];
  }

  decide(node: Node, fallbackRuntime: OmniRuntime): RuntimeEvidenceDecision {
    return chooseRuntime(this.get(node), fallbackRuntime);
  }
}

export function chooseRuntime(
  facts: RuntimeEvidenceFact[],
  fallbackRuntime: OmniRuntime,
): RuntimeEvidenceDecision {
  const allFacts = facts.length > 0 ? facts : [{
    runtime: fallbackRuntime,
    source: "fallback" as const,
    weight: EVIDENCE_WEIGHTS.fallback,
    detail: "default runtime",
  }];

  const scores = new Map<OmniRuntime, number>();
  for (const fact of allFacts) {
    scores.set(fact.runtime, (scores.get(fact.runtime) || 0) + fact.weight);
  }

  const ordered = [...scores.entries()].sort((a, b) => {
    if (b[1] !== a[1]) return b[1] - a[1];
    return a[0].localeCompare(b[0]);
  });
  const [runtime, score] = ordered[0];
  const winningFacts = allFacts.filter(f => f.runtime === runtime);
  const conflicts = allFacts.filter(f => f.runtime !== runtime);

  const confidence: RuntimeAffinity["confidence"] =
    score === EVIDENCE_WEIGHTS.fallback ? "fallback" :
    winningFacts.some(f => f.source === "runtime_tag" || f.weight === EVIDENCE_WEIGHTS.explicit) ? "definite" :
    winningFacts.some(f => f.source === "syntax") ? "definite" :
    "inferred";

  return {
    runtime,
    confidence,
    facts: winningFacts,
    conflicts,
    trace: [
      `selected ${runtime} with score ${formatScore(score)}`,
      ...winningFacts.map(f => `+ ${f.source}:${formatScore(f.weight)} ${f.detail}`),
      ...conflicts.map(f => `conflict ${f.runtime} ${f.source}:${formatScore(f.weight)} ${f.detail}`),
    ],
  };
}

export function affinityFromEvidence(decision: RuntimeEvidenceDecision): RuntimeAffinity {
  return {
    runtime: decision.runtime,
    confidence: decision.confidence,
    evidence: decision.facts.map(f => ({
      type: f.source === "global" ? "builtin" : f.source,
      detail: decision.conflicts.length > 0
        ? `${f.detail}; conflicts: ${decision.conflicts.map(c => `${c.runtime}:${c.detail}`).join(", ")}`
        : f.detail,
    })),
  };
}

function formatScore(score: number): string {
  return score === Number.POSITIVE_INFINITY ? "inf" : String(score);
}
