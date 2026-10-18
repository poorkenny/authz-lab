import { useEffect, useState } from "react";
import type { State, Strategy } from "../types";

const who = (s: string) => s.replace("user:", "");
const secs = (n: number) => `${Math.max(0, Math.floor(n))} s`;

// What happens to a stale entry under each strategy.
const STALE_FATE: Record<Strategy, string> = {
  ttl: "served until it expires",
  invalidation: "served until its invalidation is delivered (or it expires)",
  revision_aware: "never served: the revision token forces a fallback",
};

// CacheFreshness shows the cache's TTL config and how fresh each entry is:
// its age, the time left before it expires, and whether SQLite has changed
// the document since the entry was cached. Ages tick locally between fetches.
export function CacheFreshness({ state, highlight }: { state: State; highlight: boolean }) {
  const [base, setBase] = useState(() => Date.now());
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    setBase(Date.now());
    setNow(Date.now());
  }, [state]);
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const elapsed = (now - base) / 1000;

  return (
    <div className={`freshness ${highlight ? "highlight" : ""}`}>
      <div className="row spread">
        <h3>Cache TTL and freshness</h3>
        <span className="mono small">
          TTL config: <strong>{state.ttlSec} s</strong> · simulated clock t = {secs(state.clockSec + elapsed)}
        </span>
      </div>
      {state.cache.length === 0 ? (
        <p className="muted small">The cache is empty.</p>
      ) : (
        <div className="table-scroll">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">entry</th>
                <th scope="col">cached at</th>
                <th scope="col">SQLite now</th>
                <th scope="col" className="num">age</th>
                <th scope="col" className="num">expires in</th>
                <th scope="col">freshness</th>
              </tr>
            </thead>
            <tbody>
              {state.cache.map((e) => {
                const docRev = state.docRev[e.object] ?? 0;
                const stale = e.rev < docRev;
                const left = e.expiresInSec - elapsed;
                return (
                  <tr key={e.subject + e.object}>
                    <td className="mono">
                      {who(e.subject)} → {e.object}: {e.decision}
                    </td>
                    <td className="mono">rev {e.rev}</td>
                    <td className="mono">rev {docRev}</td>
                    <td className="mono num">{secs(e.ageSec + elapsed)}</td>
                    <td className="mono num">{left > 0 ? secs(left) : "expired"}</td>
                    <td className={stale ? "txt-bad" : "txt-ok"}>
                      {stale ? `✕ stale: doc changed at rev ${docRev}; ${STALE_FATE[state.strategy]}` : "✓ fresh: matches SQLite"}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
