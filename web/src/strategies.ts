import type { Strategy } from "./types";

export interface StrategyInfo {
  id: Strategy;
  name: string;
  short: string;
  how: string;
  window: string;
  read: string;
  write: string;
  needs: string;
  guarantee: string;
  verdict: "bad" | "warn" | "ok";
}

export const STRATEGIES: StrategyInfo[] = [
  {
    id: "ttl",
    name: "TTL cache (naive)",
    short: "TTL cache",
    how: "Decisions are cached until a TTL expires. Writes to SQLite don't touch the cache.",
    window: "Up to the full TTL (5 min here)",
    read: "1.2 ms hit · 16.3 ms miss",
    write: "None",
    needs: "Nothing",
    guarantee: "None: a revoked user keeps access until the TTL expires",
    verdict: "bad",
  },
  {
    id: "invalidation",
    name: "TTL + invalidation on write",
    short: "Invalidation",
    how: "After each SQLite commit an invalidation is published; the cache drops the entry when it arrives.",
    window: "Commit → invalidation arrival (80 ms here; longer under load, until TTL if the message is lost)",
    read: "1.2 ms hit · 16.3 ms miss after an invalidation",
    write: "Publish an invalidation per write (async)",
    needs: "A pub/sub channel to every cache node",
    guarantee: "Best effort: mitigates staleness, can't promise a fresh result",
    verdict: "warn",
  },
  {
    id: "revision_aware",
    name: "Revision-aware fallback",
    short: "Revision-aware",
    how: "Entries are stamped with the revision they were computed at; requests carry the doc's revision token; older entries fall back to SQLite.",
    window: "None",
    read: "1.21 ms hit · 16.31 ms fallback, once per change",
    write: "Bump the doc's token in the same SQLite transaction",
    needs: "Revision tokens propagated with requests (like Zanzibar zookies)",
    guarantee: "Every decision reflects at least the doc's latest ACL change",
    verdict: "ok",
  },
];

export const byId = Object.fromEntries(STRATEGIES.map((s) => [s.id, s])) as Record<Strategy, StrategyInfo>;
