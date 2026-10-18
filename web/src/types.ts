// Mirrors the Go types in internal/demo.

export type Strategy = "ttl" | "invalidation" | "revision_aware";

// Feature flags served by GET /api/v1/features (see docs/FEATURES.md).
export interface Features {
  controlledCacheDelivery: boolean;
  strategyStories: boolean;
  happyPathComparison: boolean;
  clientVersionToken: boolean;
}

export interface Hop {
  from: string;
  to: string;
  label: string;
  ms: number;
  kind?: "ok" | "stale" | "fallback" | "async" | "none" | "strong";
}

export interface QueueMsg {
  id: number;
  subject: string;
  object: string;
  rev: number;
  op: "touch" | "delete";
  status: "queued" | "delivered";
}

export interface CheckResult {
  id: string;
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
  pending?: QueueMsg;
  pendingPos?: number;
  queueDepth: number;
  latencyMs: number;
  hops: Hop[];
  reason: string;
  entryAgeSec: number;
  ttlSec: number;
  happyPath?: HappyPath;
  // Revision-aware only.
  tokenSource?: "latest" | "client";
  consistency?: "at_least_as_fresh" | "fully_consistent";
  latestToken?: number;
  evaluatedRev: number;
  contractMet: boolean;
}

// Revision-aware request options (FEATURE_CLIENT_VERSION_TOKEN).
export interface CheckOpts {
  token?: number;
  consistency?: "fully_consistent";
}

// The same check on the happy path: the queued invalidation was delivered first.
export interface HappyPath {
  decision: "ALLOW" | "DENY";
  correct: boolean;
  servedBy: "cache" | "primary";
  latencyMs: number;
  hops: Hop[];
  note: string;
}

export interface ClockResult {
  advancedSec: number;
  clockSec: number;
  evicted: CacheEntry[];
  note: string;
  hops: Hop[];
}

export interface WriteResult {
  op: "touch" | "delete";
  tuple: string;
  rev: number;
  token: number;
  note: string;
  hops: Hop[];
  message?: QueueMsg;
}

export interface DeliverResult {
  delivered: boolean;
  message?: QueueMsg;
  removed: boolean;
  note: string;
  hops: Hop[];
  depth: number;
}

export interface CacheEntry {
  subject: string;
  object: string;
  decision: "ALLOW" | "DENY";
  rev: number;
  ageSec: number;
  expiresInSec: number;
}

export interface State {
  strategy: Strategy;
  primaryRev: number;
  tuples: string[];
  docRev: Record<string, number>;
  cache: CacheEntry[];
  queued: QueueMsg[];
  delivered: QueueMsg[];
  ttlSec: number;
  enforceTTL: boolean;
  clockSec: number;
}

export interface CompareRow {
  strategy: Strategy;
  before: CheckResult;
  after: CheckResult;
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
  property: { seeds: number; opsPerRun: number; writes: number; deliveries: number; byStrategy: Record<Strategy, StrategyStats> };
  durationMs: number;
}
