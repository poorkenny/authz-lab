import type { Config, Outcome } from "./types";

export function defaultConfig(): Config {
  return {
    sources: {
      replicationLag: { enabled: false, lagMs: 2000 },
      decisionCache: { enabled: false, ttlMs: 30000 },
      indirectRevocation: false,
      partition: false,
      newEnemy: false,
    },
    defenses: {
      alwaysPrimary: false,
      tokens: false,
      watermark: false,
      watermarkGranularity: "object",
      revisionKeyedCache: false,
      failClosed: false,
      failMode: "forward",
      waitBudgetMs: 50,
      heartbeatTimeoutMs: 1000,
      lagBoundMs: 5000,
    },
    timing: { baseLagMs: 50, fastLaneMs: 20, primaryRttMs: 40, localMs: 0.5, cacheMs: 0.1, checkAfterRevokeMs: 200 },
  };
}

type Src = "lag" | "cache" | "indirect" | "partition" | "newEnemy";
type Def = "tokens" | "watermark" | "revCache" | "failClosed" | "deny" | "alwaysPrimary";

export function compose(src: Src[], def: Def[]): Config {
  const c = defaultConfig();
  c.sources.replicationLag.enabled = src.includes("lag");
  c.sources.decisionCache.enabled = src.includes("cache");
  c.sources.indirectRevocation = src.includes("indirect");
  c.sources.partition = src.includes("partition");
  c.sources.newEnemy = src.includes("newEnemy");
  c.defenses.tokens = def.includes("tokens");
  c.defenses.watermark = def.includes("watermark");
  c.defenses.revisionKeyedCache = def.includes("revCache");
  c.defenses.failClosed = def.includes("failClosed");
  c.defenses.failMode = def.includes("deny") ? "deny" : "forward";
  c.defenses.alwaysPrimary = def.includes("alwaysPrimary");
  return c;
}

export const GUARDED: Def[] = ["tokens", "watermark", "revCache", "failClosed"];
const ALL_SOURCES: Src[] = ["lag", "cache", "indirect", "partition", "newEnemy"];

export function presetDefenses(mode: "naive" | "tokens" | "guarded" | "full", base: Config): Config {
  const c = structuredClone(base);
  const d = c.defenses;
  d.alwaysPrimary = mode === "full";
  d.tokens = mode === "tokens" || mode === "guarded";
  d.watermark = d.revisionKeyedCache = d.failClosed = mode === "guarded";
  d.failMode = "forward";
  return c;
}

export interface Chapter {
  id: "q1" | "q2" | "q3";
  question: string;
  summary: string;
}

export const CHAPTERS: Chapter[] = [
  {
    id: "q1",
    question: "How does a stale authorization decision happen?",
    summary:
      "Low-latency checks are answered from a copy of the permission data: a replica, a cache, a snapshot. A revocation acknowledged at the primary at revision R is invisible to every copy still below R, and those copies keep answering ALLOW.",
  },
  {
    id: "q2",
    question: "How can we prevent it?",
    summary:
      "Make freshness explicit (revisions, not wall-clock time) and fail closed. Consistency tokens protect callers that hold a fresh token; a revocation watermark on a fast lane protects everyone else; revision-keyed caches and a heartbeat lease close the remaining gaps.",
  },
  {
    id: "q3",
    question: "What latency and availability trade-offs result?",
    summary:
      "Safety is paid for only by checks near a recent revocation: a short wait or a forward to the primary. Revocations are acknowledged a little later. Under a partition you choose between forwarding (latency, primary load) and denying (false denies), but never stale allows.",
  },
];

export interface ProcedureStep {
  id: string;
  chapter: Chapter["id"];
  title: string;
  goal: string;
  lookFor: string;
  kind: "drill" | "sweep";
  config?: Config;
  expected?: Outcome;
  question?: string;
  takeaway: string;
}

export const STEPS: ProcedureStep[] = [
  // Q1
  {
    id: "baseline", chapter: "q1", kind: "drill", title: "Baseline: a healthy system",
    config: compose([], []), expected: "safe",
    goal: "See the normal flow: grant, replicate, revoke, check. Every replica trails the primary by ~50 ms.",
    lookFor: "In the diagram, the changelog delivers the revoke to replica-2 ~50 ms after commit. The final check, 200 ms later, reads a snapshot that already has it.",
    takeaway: "Staleness exists even here (50 ms), but the check landed after the replica caught up. Correctness depended on timing luck, not on a guarantee.",
  },
  {
    id: "lag", chapter: "q1", kind: "drill", title: "Source 1: replication lag",
    config: compose(["lag"], []), expected: "leaked",
    goal: "Slow replica-2's changelog to 2 s and repeat the drill.",
    lookFor: "The final check is served by replica-2 with snapshot rev < revoke rev. The decision is red: ALLOW while the primary says DENY.",
    question: "The revocation was acknowledged. Why does replica-2 still say ALLOW?",
    takeaway: "Asynchronous replication means every replica serves the past. The bug isn't the lag; it's that the replica cannot tell it is answering from before a revocation.",
  },
  {
    id: "cache", chapter: "q1", kind: "drill", title: "Source 2: decision cache",
    config: compose(["cache"], []), expected: "leaked",
    goal: "Replication is healthy, but decisions are cached for 30 s.",
    lookFor: "Path = cache. The ALLOW was computed during the warm-up check, before the revoke, and served again.",
    question: "replica-2 has applied the revoke. Where does the ALLOW come from?",
    takeaway: "A TTL cache adds its whole TTL to the exposure window, even when replication is perfect.",
  },
  {
    id: "indirect", chapter: "q1", kind: "drill", title: "Source 3: indirect revocation",
    config: compose(["indirect", "lag"], []), expected: "leaked",
    goal: "Alice reaches doc:42 through group:eng → folder:7 → doc:42. Remove her from the group.",
    lookFor: "The revoked tuple is on group:eng, not doc:42. The resolution path of the stale ALLOW shows the old membership.",
    question: "Which object's permissions changed, and which tuple was written?",
    takeaway: "Revocations propagate through the relation graph. Invalidation keyed on the checked object misses them.",
  },
  {
    id: "partition", chapter: "q1", kind: "drill", title: "Source 4: partitioned replica",
    config: compose(["partition"], []), expected: "leaked",
    goal: "Cut replica-2 off from the network 2 s before the revoke.",
    lookFor: "Blocked deliveries (red) to replica-2. It never learns about the revoke but keeps answering confidently.",
    takeaway: "Without a liveness signal, a partitioned replica is stale indefinitely, and nothing in its answers tells you so.",
  },
  {
    id: "new-enemy", chapter: "q1", kind: "drill", title: "Source 5: the \"new enemy\"",
    config: compose(["lag", "newEnemy"], []), expected: "leaked",
    goal: "After removing Alice, Bob writes secret content v2 to doc:42. Alice then reads it.",
    lookFor: "Content v2 was written after the revoke, yet Alice is ALLOWED. She sees content that was never shared with her.",
    question: "Why is this worse than just 'access lasted a bit longer'?",
    takeaway: "The new-enemy problem (Zanzibar §2.2): applying a stale ACL to new content leaks data the owner created after excluding the user. Staleness is a correctness bug, not a delay.",
  },
  // Q2
  {
    id: "tokens", chapter: "q2", kind: "drill", title: "Defense 1: consistency tokens",
    config: compose(["lag", "newEnemy"], ["tokens"]), expected: "safe",
    goal: "Content v2 stores the token taken when it was written. The check must use a snapshot at least that fresh.",
    lookFor: "required rev = token rev ≥ revoke rev. replica-2 can't meet it, so the check waits or forwards: path waited/forwarded.",
    takeaway: "Tokens (Zanzibar zookies) solve the new-enemy problem for content: the content carries the revision its ACL check must reflect.",
  },
  {
    id: "tokens-gap", chapter: "q2", kind: "drill", title: "Tokens are not enough",
    config: compose(["lag"], ["tokens"]), expected: "leaked",
    goal: "Same lag, but Alice's client still holds the token it got before the revoke.",
    lookFor: "token rev < revoke rev, so at_least(token) is satisfied by the stale snapshot. Still a leak.",
    question: "Who holds a token that postdates the revocation? Who doesn't?",
    takeaway: "Tokens protect only callers that learned about the revocation. The revoked user, other services and old sessions don't, so the server needs its own fence.",
  },
  {
    id: "watermark", chapter: "q2", kind: "drill", title: "Defense 2: revocation watermark",
    config: compose(["indirect", "lag"], ["watermark"]), expected: "safe",
    goal: "Each revoke publishes a tiny watermark on a fast lane, expanded to dependents (group → folder → doc).",
    lookFor: "The watermark reaches replica-2 ~20 ms after commit, long before the 2 s changelog. The check sees required rev = watermark and refuses its stale snapshot.",
    takeaway: "The fast lane carries only (object, revision), so it outruns bulk replication. Grants stay eventually consistent; revocations become effectively strict, even for tokenless callers.",
  },
  {
    id: "cache-gap", chapter: "q2", kind: "drill", title: "A TTL cache bypasses the fence",
    config: compose(["cache"], ["watermark"]), expected: "leaked",
    goal: "Watermark on, but the decision cache is keyed only by TTL.",
    lookFor: "path = cache: the watermark was known but never consulted.",
    takeaway: "Every layer that can answer a check must honor the fence, caches included.",
  },
  {
    id: "rev-cache", chapter: "q2", kind: "drill", title: "Defense 3: revision-keyed cache",
    config: compose(["cache"], ["watermark", "revCache"]), expected: "safe",
    goal: "Cache entries remember the revision they were computed at and are valid only if ≥ the required revision.",
    lookFor: "Note: 'cache entry at rev … below required rev …: bypassed'.",
    takeaway: "TTL becomes a memory bound, not the safety mechanism.",
  },
  {
    id: "partition-gap", chapter: "q2", kind: "drill", title: "Partitions cut the fast lane too",
    config: compose(["partition"], ["watermark"]), expected: "leaked",
    goal: "Watermark on, but replica-2 is partitioned.",
    lookFor: "Both the changelog and the watermark are blocked. replica-2 has no way to know.",
    takeaway: "Any push-based fence fails silently under a partition. The replica must also notice that it can't hear the primary.",
  },
  {
    id: "fail-closed", chapter: "q2", kind: "drill", title: "Defense 4: fail closed on lost heartbeat",
    config: compose(["partition"], ["watermark", "failClosed"]), expected: "safe",
    goal: "The fast lane carries heartbeats. A replica that hasn't heard one within 1 s stops answering locally and forwards.",
    lookFor: "defense = fail_closed, path = forwarded. Had replica-2's lease still been valid at revoke time, the primary would have held the acknowledgment until it expired.",
    takeaway: "Heartbeat lease + fail closed = a partitioned replica can't serve stale data longer than the lease, and the primary never acknowledges a revocation before that's guaranteed.",
  },
  {
    id: "guarded", chapter: "q2", kind: "drill", title: "All sources vs. the guarded design",
    config: compose(ALL_SOURCES, GUARDED), expected: "safe",
    goal: "Turn on every staleness source and every defense.",
    lookFor: "Alice is denied, and Bob's control check still succeeds via the primary. Try toggling single defenses off in the composer to see which gap reopens.",
    takeaway: "Each defense closes a specific gap. Together they satisfy the invariant: no ALLOW from a snapshot older than an acknowledged revocation.",
  },
  // Q3
  {
    id: "sweep", chapter: "q3", kind: "sweep", title: "Latency vs. replication lag",
    goal: "Replay the same randomized workload (3% revokes, 30% of checks from just-revoked users) under four modes at lags from 0 to 5 s.",
    lookFor: "guarded: p50 stays at local speed and stale allows stay at 0. Only the share of checks forwarded to the primary grows with lag. full: always safe, but every check pays the primary RTT. naive: fast and leaking.",
    takeaway: "The cost of safety scales with (revocation rate × lag), not with total traffic. Most checks never pay it.",
  },
  {
    id: "availability", chapter: "q3", kind: "drill", title: "Availability: fail-closed deny",
    config: compose(["partition"], ["watermark", "failClosed", "deny"]), expected: "safe_false_deny",
    goal: "Same partition, but fail-closed denies instead of forwarding (e.g. when the primary is also unreachable).",
    lookFor: "Alice is denied, correctly. But Bob, a legitimate owner, is denied too: a false deny.",
    question: "Which is worse for your product during a partition: false denies, or extra latency and load on the primary?",
    takeaway: "This is CAP made explicit for security: under a partition you trade availability, never correctness. Forward mode keeps availability as long as the primary is reachable.",
  },
  {
    id: "full", chapter: "q3", kind: "drill", title: "Fully consistent: the expensive fix",
    config: compose(ALL_SOURCES, ["alwaysPrimary"]), expected: "safe",
    goal: "Send every check to the primary.",
    lookFor: "Safe, but every check (Bob's too) costs the primary RTT, and the primary takes 100% of the read load.",
    takeaway: "Correct but unscalable. The guarded design gets the same safety while serving most checks locally.",
  },
];
