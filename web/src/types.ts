// Mirrors the Go types in internal/demo.

export type Mode = "plain" | "revision_aware";

export interface Hop {
  from: string;
  to: string;
  label: string;
  ms: number;
  kind?: "ok" | "stale" | "fallback" | "lost";
}

export interface CheckResult {
  id: string;
  mode: Mode;
  subject: string;
  object: string;
  decision: "ALLOW" | "DENY";
  expected: "ALLOW" | "DENY";
  correct: boolean;
  servedBy: "cache" | "primary";
  cacheRev: number;
  requiredRev: number;
  primaryRev: number;
  latencyMs: number;
  hops: Hop[];
  reason: string;
}

export interface WriteResult {
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
  ttlSec: number;
}

export interface State {
  mode: Mode;
  primaryRev: number;
  tuples: string[];
  docRev: Record<string, number>;
  cache: CacheEntry[];
}

export interface TestCase {
  name: string;
  kind: "scenario" | "property";
  description: string;
  passed: boolean;
  details: string[];
}

export interface ModeStats {
  checks: number;
  wrong: number;
  staleAllows: number;
  cacheHits: number;
  fallbacks: number;
  meanMs: number;
  hitRatePct: number;
}

export interface SuiteResult {
  passed: boolean;
  cases: TestCase[];
  property: { seeds: number; opsPerRun: number; writes: number; plain: ModeStats; revisionAware: ModeStats };
  durationMs: number;
}
