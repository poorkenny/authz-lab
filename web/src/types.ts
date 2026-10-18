// Mirrors the Go API types in internal/sim and internal/lab.

export interface Config {
  sources: {
    replicationLag: { enabled: boolean; lagMs: number };
    decisionCache: { enabled: boolean; ttlMs: number };
    indirectRevocation: boolean;
    partition: boolean;
    newEnemy: boolean;
  };
  defenses: {
    alwaysPrimary: boolean;
    tokens: boolean;
    watermark: boolean;
    watermarkGranularity: "object" | "namespace";
    revisionKeyedCache: boolean;
    failClosed: boolean;
    failMode: "forward" | "deny";
    waitBudgetMs: number;
    heartbeatTimeoutMs: number;
    lagBoundMs: number;
  };
  timing: {
    baseLagMs: number;
    fastLaneMs: number;
    primaryRttMs: number;
    localMs: number;
    cacheMs: number;
    checkAfterRevokeMs: number;
  };
}

export type Path = "primary" | "local" | "cache" | "waited" | "forwarded" | "fail_closed";

export interface Evidence {
  servedBy: string;
  snapshotRev: number;
  requiredRev: number;
  tokenRev: number;
  watermarkRev: number;
  primaryRevAtStart: number;
  path: Path;
  latencyMs: number;
  defenseApplied?: string;
  resolution: string[];
  notes: string[];
}

export interface CheckResult {
  checkId: string;
  subject: string;
  permission: string;
  object: string;
  decision: "ALLOW" | "DENY";
  oracleDecision: "ALLOW" | "DENY";
  stale: boolean;
  falseDeny: boolean;
  evidence: Evidence;
  t: number;
}

export interface LabEvent {
  seq: number;
  t: number;
  kind: string;
  from: string;
  to: string;
  label: string;
  rev?: number;
  severity?: "info" | "ok" | "warn" | "danger";
  checkId?: string;
}

export interface Step {
  t: number;
  kind: string;
  title: string;
  detail?: string;
  rev?: number;
  check?: CheckResult;
}

export type Outcome = "leaked" | "safe" | "safe_false_deny";

export interface Verdict {
  outcome: Outcome;
  leaked: boolean;
  falseDeny: boolean;
  headline: string;
  causes: string[];
  defenses: string[];
  tradeoffs: string[];
  finalLatencyMs: number;
}

export interface DrillResult {
  runId: string;
  config: Config;
  steps: Step[];
  final: CheckResult;
  control: CheckResult;
  revokeRev: number;
  revokeAckMs: number;
  verdict: Verdict;
  timeline: LabEvent[];
}

export type Mode = "naive" | "tokens" | "guarded" | "full";

export interface SweepPoint {
  mode: Mode;
  lagMs: number;
  checks: number;
  p50Ms: number;
  p99Ms: number;
  meanMs: number;
  paths: Record<string, number>;
  localPct: number;
  primaryLoadPct: number;
  staleAllows: number;
  falseDenies: number;
  revokeAckMs: number;
}

export interface SweepResult {
  points: SweepPoint[];
}

export interface AuditRecord {
  id: string;
  runId?: string;
  t: number;
  kind: "write" | "check";
  subject?: string;
  permission?: string;
  object?: string;
  tuple?: string;
  op?: string;
  rev?: number;
  decision?: string;
  oracleDecision?: string;
  stale: boolean;
  falseDeny: boolean;
  servedBy?: string;
  snapshotRev?: number;
  requiredRev?: number;
  watermarkRev?: number;
  primaryRev?: number;
  path?: string;
  latencyMs?: number;
  defenseApplied?: string;
  notes?: string[];
}
