// Mirrors the Go types in internal/demo.

export type Strategy = "ttl" | "invalidation" | "revision_aware";

export interface Hop {
  from: string;
  to: string;
  label: string;
  ms: number;
  kind?: "ok" | "stale" | "fallback" | "async" | "none";
}

export interface InvalidationMsg {
  object: string;
  rev: number;
  sentAt: number;
  deliverAt: number;
}

export interface CheckResult {
  id: string;
  t: number;
  strategy: Strategy;
  subject: string;
  object: string;
  decision: "ALLOW" | "DENY";
  expected: "ALLOW" | "DENY";
  correct: boolean;
  servedBy: "cache" | "primary";
  cacheRev: number;
  requiredRev: number;
  primaryRev: number;
  raceWindow: boolean;
  invalidation?: InvalidationMsg;
  latencyMs: number;
  hops: Hop[];
  reason: string;
}

export interface WriteResult {
  t: number;
  op: "touch" | "delete";
  tuple: string;
  rev: number;
  token: number;
  note: string;
  hops: Hop[];
}

export interface CacheEntry {
  subject: string;
  object: string;
  decision: "ALLOW" | "DENY";
  rev: number;
  cachedAt: number;
  expiresAt: number;
}

export interface State {
  strategy: Strategy;
  now: number;
  primaryRev: number;
  tuples: string[];
  docRev: Record<string, number>;
  cache: CacheEntry[];
  pending: InvalidationMsg[];
}

export interface CompareRow {
  strategy: Strategy;
  soon: CheckResult;
  later: CheckResult;
}

export interface TestCase {
  name: string;
  kind: "scenario" | "property";
  strategy?: Strategy;
  description: string;
  passed: boolean;
  details: string[];
}

export interface StrategyStats {
  checks: number;
  wrong: number;
  staleAllows: number;
  inWindow: number;
  cacheHits: number;
  fallbacks: number;
  meanMs: number;
  hitRatePct: number;
}

export interface SuiteResult {
  passed: boolean;
  cases: TestCase[];
  property: { seeds: number; opsPerRun: number; writes: number; byStrategy: Record<Strategy, StrategyStats> };
  durationMs: number;
}
