import { useEffect, useState } from "react";
import { api } from "./api";
import type { CheckResult, Hop, Mode, State, WriteResult } from "./types";
import { FlowDiagram, fmtMs, shortLabel } from "./components/FlowDiagram";
import { TestsPanel } from "./components/TestsPanel";

type Entry = { kind: "check"; r: CheckResult } | { kind: "write"; r: WriteResult };

const STEPS = [
  { title: "Alice opens doc:42", hint: "Cache miss → evaluated at the primary → ALLOW is cached." },
  { title: "Admin revokes Alice", hint: "The primary commits the revoke. The invalidation to the cache is lost." },
  { title: "Alice opens doc:42 again", hint: "Does the cache still say ALLOW?" },
];

// Latency of each path through the system (sum of the modelled hops).
const PATHS = [
  { id: "plain-hit", name: "Plain cache hit", hops: "request 0.5 + lookup 0.2 + response 0.5", ms: 1.2 },
  { id: "aware-hit", name: "Revision-aware hit (entry fresh)", hops: "+ revision check 0.01", ms: 1.21 },
  { id: "aware-fallback", name: "Revision-aware fallback (entry stale)", hops: "+ primary 15 + cache refresh 0.1", ms: 16.31 },
  { id: "miss", name: "Cache miss (either mode)", hops: "request + lookup + primary 15 + refresh + response", ms: 16.3 },
];

function pathOf(r: CheckResult): string {
  if (r.servedBy === "cache") return r.mode === "plain" ? "plain-hit" : "aware-hit";
  return r.cacheRev > 0 ? "aware-fallback" : "miss";
}

const MODE_LABEL: Record<Mode, string> = { plain: "Plain cache", revision_aware: "Revision-aware fallback" };

export default function App() {
  const [state, setState] = useState<State | null>(null);
  const [step, setStep] = useState(0);
  const [log, setLog] = useState<Entry[]>([]);
  const [hops, setHops] = useState<Hop[]>([]);
  const [actor, setActor] = useState("Client (Alice)");
  const [finals, setFinals] = useState<Partial<Record<Mode, CheckResult>>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [user, setUser] = useState("user:alice");
  const [doc, setDoc] = useState("doc:42");

  useEffect(() => {
    api.state().then(setState).catch((e: Error) => setError(e.message));
  }, []);

  async function act(f: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await f();
      setState(await api.state());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const check = (subject: string, object: string) =>
    act(async () => {
      const r = await api.check(subject, object);
      setLog((l) => [{ kind: "check" as const, r }, ...l].slice(0, 12));
      setHops(r.hops);
      setActor(`Client (${subject.replace("user:", "")})`);
    });
  const write = (op: "grant" | "revoke", subject: string, object: string) =>
    act(async () => {
      const r = await api[op](subject, object);
      setLog((l) => [{ kind: "write" as const, r }, ...l].slice(0, 12));
      setHops(r.hops);
    });

  async function runStep(i: number) {
    if (i === 0) await check("user:alice", "doc:42");
    if (i === 1) await write("revoke", "user:alice", "doc:42");
    if (i === 2) {
      await act(async () => {
        const r = await api.check("user:alice", "doc:42");
        setLog((l) => [{ kind: "check" as const, r }, ...l].slice(0, 12));
        setHops(r.hops);
        setActor("Client (alice)");
        setFinals((f) => ({ ...f, [r.mode]: r }));
      });
    }
    setStep(i + 1);
  }

  const reset = (mode?: Mode) =>
    act(async () => {
      if (mode) await api.setMode(mode);
      await api.reset();
      setStep(0);
      setLog([]);
      setHops([]);
    });

  const last = log.find((e) => e.kind === "check")?.r as CheckResult | undefined;
  const lastEntry = log[0];
  const mode = state?.mode ?? "plain";

  return (
    <div className="app">
      <header>
        <h1>Stale Cache Revocation Demo</h1>
        <p className="lede">
          How one stale permission cache lets a revoked user back in, and how a <strong>revision-aware fallback</strong> keeps checks fast <em>and</em> correct.
        </p>
      </header>

      <section className="card" aria-labelledby="problem-h">
        <h2 id="problem-h">1 · The problem: one stale cache</h2>
        <p>
          Permission checks are cached so they're fast. When an admin revokes access, the cache is supposed to be invalidated. But invalidations get lost or
          delayed, and until the TTL expires (here 5 minutes), the cache keeps answering <strong>ALLOW</strong> for a user who no longer has access.
        </p>
      </section>

      <section className="card" aria-labelledby="demo-h">
        <h2 id="demo-h">2 · Interactive demonstration</h2>

        <div className="mode-row">
          <span className="muted">Check API mode:</span>
          <div className="segmented" role="radiogroup" aria-label="Check API mode">
            {(["plain", "revision_aware"] as Mode[]).map((m) => (
              <button key={m} role="radio" aria-checked={mode === m} className={mode === m ? "on" : ""} disabled={busy} onClick={() => mode !== m && reset(m)}>
                {MODE_LABEL[m]}
              </button>
            ))}
          </div>
          <button className="link" onClick={() => reset()} disabled={busy}>
            Reset
          </button>
        </div>

        <ol className="steps">
          {STEPS.map((s, i) => (
            <li key={s.title} className={i < step ? "done" : i === step ? "next" : ""}>
              <button className={i === step ? "primary-btn" : ""} disabled={busy || i > step} onClick={() => runStep(i)}>
                <span className="step-n">{i < step ? "✓" : i + 1}</span> {s.title}
              </button>
              <span className="muted small">{s.hint}</span>
            </li>
          ))}
        </ol>
        {error && <p className="txt-bad">{error}</p>}

        <FlowDiagram hops={hops} state={state} actor={actor} />

        {lastEntry && (
          <div className="result-row">
            {lastEntry.kind === "check" ? (
              <div className={`result ${lastEntry.r.correct ? "ok" : "bad"}`} aria-live="polite">
                <div className="decision">
                  {lastEntry.r.subject.replace("user:", "")} → {lastEntry.r.object}: <strong>{lastEntry.r.decision}</strong>
                  <span className={`badge ${lastEntry.r.correct ? "ok" : "bad"}`}>
                    {lastEntry.r.correct ? "✓ correct" : `✕ stale: the primary says ${lastEntry.r.expected}`}
                  </span>
                </div>
                <p>{lastEntry.r.reason}</p>
                <p className="mono small">
                  served by {lastEntry.r.servedBy} · cache entry rev {lastEntry.r.cacheRev || "—"} · revision token {lastEntry.r.requiredRev || "not used"} · primary rev{" "}
                  {lastEntry.r.primaryRev} · {fmtMs(lastEntry.r.latencyMs)} ms
                </p>
              </div>
            ) : (
              <div className="result neutral" aria-live="polite">
                <div className="decision">
                  {lastEntry.r.op === "delete" ? "Revoked" : "Granted"} <span className="mono">{lastEntry.r.tuple}</span>
                </div>
                <p>{lastEntry.r.note}</p>
              </div>
            )}
          </div>
        )}

        <div className="two-col">
          <div>
            <h3>Hops of the last request</h3>
            {hops.length === 0 ? (
              <p className="muted">No request yet.</p>
            ) : (
              <table className="data">
                <thead>
                  <tr>
                    <th scope="col">#</th>
                    <th scope="col">hop</th>
                    <th scope="col">detail</th>
                    <th scope="col" className="num">ms</th>
                  </tr>
                </thead>
                <tbody>
                  {hops.map((h, i) => (
                    <tr key={i} className={h.kind ? `k-${h.kind}` : ""}>
                      <td className="mono">{i + 1}</td>
                      <td>{shortLabel(h, i, hops)}</td>
                      <td className="small">{h.label}</td>
                      <td className="mono num">{h.kind === "lost" ? "—" : fmtMs(h.ms)}</td>
                    </tr>
                  ))}
                  <tr className="total-row">
                    <td></td>
                    <td colSpan={2}>total (critical path)</td>
                    <td className="mono num">{fmtMs(hops.reduce((a, h) => a + h.ms, 0))}</td>
                  </tr>
                </tbody>
              </table>
            )}
          </div>
          <div>
            <h3>Latency budget per path</h3>
            <table className="data">
              <tbody>
                {PATHS.map((p) => (
                  <tr key={p.id} className={last && pathOf(last) === p.id && lastEntry?.kind === "check" ? "current" : ""}>
                    <th scope="row">
                      {p.name}
                      <div className="muted small">{p.hops}</div>
                    </th>
                    <td className="mono num">{fmtMs(p.ms)} ms</td>
                  </tr>
                ))}
              </tbody>
            </table>
            <p className="muted small">
              The revision check itself is nearly free. The cost is the fallback: only checks that find a <em>stale</em> entry pay the extra primary round
              trip, and they pay it once, because the fallback refreshes the cache.
            </p>
          </div>
        </div>

        {(finals.plain || finals.revision_aware) && (
          <div className="compare">
            <h3>Step 3 compared across modes</h3>
            <table className="data">
              <thead>
                <tr>
                  <th scope="col">mode</th>
                  <th scope="col">Alice after revoke</th>
                  <th scope="col">correct?</th>
                  <th scope="col">served by</th>
                  <th scope="col" className="num">latency</th>
                </tr>
              </thead>
              <tbody>
                {(["plain", "revision_aware"] as Mode[]).map((m) => {
                  const r = finals[m];
                  return (
                    <tr key={m}>
                      <th scope="row">{MODE_LABEL[m]}</th>
                      {r ? (
                        <>
                          <td className="mono">{r.decision}</td>
                          <td className={r.correct ? "txt-ok" : "txt-bad"}>{r.correct ? "✓ yes" : "✕ stale ALLOW"}</td>
                          <td>{r.servedBy}</td>
                          <td className="mono num">{fmtMs(r.latencyMs)} ms</td>
                        </>
                      ) : (
                        <td colSpan={4} className="muted">
                          switch to this mode and run the three steps
                        </td>
                      )}
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}

        <details className="explore">
          <summary>Explore freely</summary>
          <div className="row">
            <select value={user} onChange={(e) => setUser(e.target.value)} aria-label="User">
              <option value="user:alice">Alice</option>
              <option value="user:bob">Bob (owner)</option>
              <option value="user:carol">Carol</option>
            </select>
            <select value={doc} onChange={(e) => setDoc(e.target.value)} aria-label="Document">
              <option value="doc:42">doc:42</option>
              <option value="doc:7">doc:7</option>
            </select>
            <button disabled={busy} onClick={() => check(user, doc)}>
              Check view
            </button>
            <button disabled={busy} onClick={() => write("grant", user, doc)}>
              Grant viewer
            </button>
            <button disabled={busy} onClick={() => write("revoke", user, doc)}>
              Revoke viewer
            </button>
          </div>
          {state && (
            <p className="mono small">
              primary tuples (rev {state.primaryRev}): {state.tuples.join(", ") || "none"}
            </p>
          )}
          {log.length > 0 && (
            <ul className="history">
              {log.map((e, i) => (
                <li key={i} className="small">
                  {e.kind === "check" ? (
                    <>
                      <span className={e.r.correct ? "txt-ok" : "txt-bad"}>{e.r.correct ? "✓" : "✕"}</span> {MODE_LABEL[e.r.mode]}: {e.r.subject} view {e.r.object} →{" "}
                      {e.r.decision} via {e.r.servedBy} ({fmtMs(e.r.latencyMs)} ms)
                    </>
                  ) : (
                    <>
                      ✎ {e.r.op} {e.r.tuple} → rev {e.r.rev}
                    </>
                  )}
                </li>
              ))}
            </ul>
          )}
        </details>
      </section>

      <section className="card" aria-labelledby="fix-h">
        <h2 id="fix-h">3 · The fix: revision-aware fallback</h2>
        <div className="two-col">
          <pre className="code">{`required = doc.revisionToken      // rev of doc's last ACL change
entry    = cache.get(user, doc)

if entry and entry.rev >= required:
    return entry.decision         // fast path: fresh enough

decision = primary.check(user, doc)   // fallback: source of truth
cache.put(user, doc, decision, rev = primary.rev)
return decision`}</pre>
          <ul>
            <li>
              Every cache entry is stamped with the <strong>revision</strong> it was computed at.
            </li>
            <li>
              Every ACL change gives the document a new <strong>revision token</strong> that travels with requests (like a Zanzibar <em>zookie</em>).
            </li>
            <li>
              An entry older than the token is never served, even if the invalidation was lost and the TTL hasn't expired.
            </li>
            <li>
              <strong>Trade-off:</strong> a fallback costs one primary round trip (~15 ms vs ~1.2 ms), paid only by the first check after an ACL change. All
              other checks stay on the fast path.
            </li>
          </ul>
        </div>
      </section>

      <TestsPanel />

      <footer className="muted small">Mock system with modelled latencies. Each browser gets its own isolated demo session.</footer>
    </div>
  );
}
