import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import type { CheckResult, CompareRow, Hop, State, Strategy, WriteResult } from "./types";
import { FlowDiagram, fmtMs, hopMs, shortLabel } from "./components/FlowDiagram";
import { RaceTimeline } from "./components/RaceTimeline";
import { TestsPanel } from "./components/TestsPanel";
import { STRATEGIES, byId } from "./strategies";

type Entry = { kind: "check"; r: CheckResult } | { kind: "write"; r: WriteResult };

interface StepDef {
  title: string;
  hint: string;
  run: (a: Actions) => Promise<void>;
}
interface Actions {
  check: (subject: string, object: string, waitMs?: number) => Promise<void>;
  write: (op: "grant" | "revoke", subject: string, object: string, waitMs?: number) => Promise<void>;
}

const STEPS: StepDef[] = [
  { title: "Alice opens doc:42", hint: "Cache miss → SQLite → ALLOW is cached.", run: (a) => a.check("user:alice", "doc:42") },
  { title: "1 s later, admin revokes Alice", hint: "SQLite commits the revoke.", run: (a) => a.write("revoke", "user:alice", "doc:42", 1000) },
  { title: "Alice retries 10 ms later", hint: "Inside the invalidation's 80 ms propagation window.", run: (a) => a.check("user:alice", "doc:42", 10) },
  { title: "Alice retries 1 s later", hint: "After the invalidation has arrived.", run: (a) => a.check("user:alice", "doc:42", 1000) },
];

// Latency of each path through the system (sum of the modelled hops).
const PATHS = [
  { id: "hit", name: "Cache hit (TTL / invalidation)", hops: "request 0.5 + lookup 0.2 + response 0.5", ms: "1.2 ms" },
  { id: "ra-hit", name: "Revision-aware hit (entry fresh)", hops: "+ revision check 0.01", ms: "1.21 ms" },
  { id: "miss", name: "Cache miss → SQLite (incl. after an invalidation)", hops: "+ SQLite query 15 + cache refresh 0.1", ms: "16.3 ms" },
  { id: "ra-fallback", name: "Revision-aware fallback (entry stale)", hops: "+ revision check + SQLite 15 + refresh", ms: "16.31 ms" },
  { id: "write", name: "Write: SQLite commit", hops: "invalidation travels async (+80 ms), off the write path", ms: "15 ms" },
];

function pathOf(r: CheckResult): string {
  if (r.servedBy === "cache") return r.strategy === "revision_aware" ? "ra-hit" : "hit";
  return r.cacheRev > 0 && r.strategy === "revision_aware" ? "ra-fallback" : "miss";
}

export default function App() {
  const [state, setState] = useState<State | null>(null);
  const [step, setStep] = useState(0);
  const [log, setLog] = useState<Entry[]>([]);
  const [hops, setHops] = useState<Hop[]>([]);
  const [actor, setActor] = useState("Client (alice)");
  const [rows, setRows] = useState<CompareRow[]>([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [user, setUser] = useState("user:alice");
  const [doc, setDoc] = useState("doc:42");
  const demoRef = useRef<HTMLElement>(null);

  useEffect(() => {
    api.state().then(setState).catch((e: Error) => setError(e.message));
    api
      .compare()
      .then((c) => setRows(c.rows))
      .catch(() => {});
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

  const actions: Actions = {
    check: (subject, object, waitMs = 0) =>
      act(async () => {
        const r = await api.check({ subject, object, waitMs });
        setLog((l) => [{ kind: "check" as const, r }, ...l].slice(0, 12));
        setHops(r.hops);
        setActor(`Client (${subject.replace("user:", "")})`);
      }),
    write: (op, subject, object, waitMs = 0) =>
      act(async () => {
        const r = await api[op]({ subject, object, waitMs });
        setLog((l) => [{ kind: "write" as const, r }, ...l].slice(0, 12));
        setHops(r.hops);
      }),
  };

  async function runStep(i: number) {
    await STEPS[i].run(actions);
    setStep(i + 1);
  }

  const switchTo = (s: Strategy) =>
    act(async () => {
      await api.setStrategy(s);
      setStep(0);
      setLog([]);
      setHops([]);
    });

  const strategy = state?.strategy ?? "ttl";
  const lastEntry = log[0];
  const lastCheck = lastEntry?.kind === "check" ? lastEntry.r : undefined;

  return (
    <div className="app">
      <header>
        <p className="eyebrow">Authorization showcase</p>
        <h1>Stale permissions: three strategies, three trade-offs</h1>
        <div className="goal" role="note">
          <strong>Goal of this demo:</strong> compare strategies for stopping a permission cache from serving <strong>stale ALLOWs</strong> after access is
          revoked. For each one, see what it fixes, what it can't promise, and what it costs in latency. That makes it easier to choose the right trade-off.
        </div>
      </header>

      <section className="card" aria-labelledby="strategies-h">
        <h2 id="strategies-h">1 · The problem and the strategies</h2>
        <p>
          Permission checks are cached for speed. When an admin revokes access in the primary store (SQLite), any cached ALLOW for that user is now wrong. How
          long can it keep being served?
        </p>
        <div className="strategy-grid">
          {STRATEGIES.map((s, i) => (
            <article key={s.id} className={`strategy-card verdict-${s.verdict} ${strategy === s.id ? "selected" : ""}`}>
              <header>
                <span className="strategy-n">{i + 1}</span>
                <h3>{s.name}</h3>
              </header>
              <p>{s.how}</p>
              <dl>
                <dt>Stale window</dt>
                <dd>{s.window}</dd>
                <dt>Read latency</dt>
                <dd>{s.read}</dd>
                <dt>Write cost</dt>
                <dd>{s.write}</dd>
                <dt>Needs</dt>
                <dd>{s.needs}</dd>
              </dl>
              <p className={`guarantee verdict-${s.verdict}`}>
                <span aria-hidden="true">{s.verdict === "ok" ? "✓" : s.verdict === "warn" ? "!" : "✕"}</span> {s.guarantee}
              </p>
              <button
                onClick={() => {
                  if (strategy !== s.id) switchTo(s.id);
                  demoRef.current?.scrollIntoView({ behavior: "smooth" });
                }}
              >
                Try it below
              </button>
            </article>
          ))}
        </div>
      </section>

      <section className="card" aria-labelledby="demo-h" ref={demoRef}>
        <h2 id="demo-h">2 · Interactive demonstration</h2>

        <div className="mode-row">
          <span className="muted">Strategy:</span>
          <div className="segmented" role="radiogroup" aria-label="Strategy">
            {STRATEGIES.map((s) => (
              <button key={s.id} role="radio" aria-checked={strategy === s.id} className={strategy === s.id ? "on" : ""} disabled={busy} onClick={() => strategy !== s.id && switchTo(s.id)}>
                {s.short}
              </button>
            ))}
          </div>
          <button className="link" onClick={() => switchTo(strategy)} disabled={busy}>
            Reset
          </button>
        </div>

        <ol className="steps">
          {STEPS.map((s, i) => (
            <li key={s.title} className={i < step ? "done" : ""}>
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
                  t={lastEntry.r.t} ms · {lastEntry.r.subject.replace("user:", "")} → {lastEntry.r.object}: <strong>{lastEntry.r.decision}</strong>
                  <span className={`badge ${lastEntry.r.correct ? "ok" : "bad"}`}>
                    {lastEntry.r.correct ? "✓ fresh" : `✕ stale: SQLite says ${lastEntry.r.expected}`}
                  </span>
                  {lastEntry.r.raceWindow && <span className="badge warn">invalidation in flight</span>}
                </div>
                <p>{lastEntry.r.reason}</p>
                <p className="mono small">
                  served by {lastEntry.r.servedBy} · cache entry rev {lastEntry.r.cacheRev || "—"} · revision token {lastEntry.r.requiredRev || "not used"} · SQLite
                  rev {lastEntry.r.primaryRev} · {fmtMs(lastEntry.r.latencyMs)} ms
                </p>
              </div>
            ) : (
              <div className="result neutral" aria-live="polite">
                <div className="decision">
                  t={lastEntry.r.t} ms · {lastEntry.r.op === "delete" ? "Revoked" : "Granted"} <span className="mono">{lastEntry.r.tuple}</span>
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
                    <th scope="col" className="num">latency</th>
                  </tr>
                </thead>
                <tbody>
                  {hops.map((h, i) => (
                    <tr key={i} className={h.kind ? `k-${h.kind}` : ""}>
                      <td className="mono">{i + 1}</td>
                      <td>{shortLabel(h, i, hops)}</td>
                      <td className="small">{h.label}</td>
                      <td className="mono num">{hopMs(h)}</td>
                    </tr>
                  ))}
                  <tr className="total-row">
                    <td></td>
                    <td colSpan={2}>critical path</td>
                    <td className="mono num">{fmtMs(hops.reduce((a, h) => a + (h.kind === "async" ? 0 : h.ms), 0))} ms</td>
                  </tr>
                </tbody>
              </table>
            )}
          </div>
          <div>
            <h3>Latency budget per path</h3>
            <table className="data">
              <tbody>
                {PATHS.map((p) => {
                  const current = lastEntry ? (lastEntry.kind === "write" ? p.id === "write" : lastCheck && pathOf(lastCheck) === p.id) : false;
                  return (
                    <tr key={p.id} className={current ? "current" : ""}>
                      <th scope="row">
                        {p.name}
                        <div className="muted small">{p.hops}</div>
                      </th>
                      <td className="mono num">{p.ms}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>

        <div className="compare">
          <div className="row spread">
            <h3>Same story, all three strategies</h3>
            <button disabled={busy} onClick={() => api.compare().then((c) => setRows(c.rows))}>
              Re-run comparison
            </button>
          </div>
          <div className="table-scroll">
            <table className="data">
              <thead>
                <tr>
                  <th scope="col">strategy</th>
                  <th scope="col">10 ms after revoke</th>
                  <th scope="col">1 s after revoke</th>
                  <th scope="col">verdict</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.strategy} className={r.strategy === strategy ? "current" : ""}>
                    <th scope="row">{byId[r.strategy].name}</th>
                    {[r.soon, r.later].map((c, i) => (
                      <td key={i} className={c.correct ? "txt-ok" : "txt-bad"}>
                        {c.correct ? "✓" : "✕"} {c.decision} <span className="muted mono small">({c.servedBy}, {fmtMs(c.latencyMs)} ms)</span>
                      </td>
                    ))}
                    <td>{r.soon.correct && r.later.correct ? "always fresh" : r.later.correct ? "mitigated: stale inside the window" : "stale until TTL"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

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
            <button disabled={busy} onClick={() => actions.check(user, doc)}>
              Check now
            </button>
            <button disabled={busy} onClick={() => actions.check(user, doc, 10)}>
              Check in 10 ms
            </button>
            <button disabled={busy} onClick={() => actions.check(user, doc, 1000)}>
              Check in 1 s
            </button>
            <button disabled={busy} onClick={() => actions.write("grant", user, doc)}>
              Grant viewer
            </button>
            <button disabled={busy} onClick={() => actions.write("revoke", user, doc)}>
              Revoke viewer
            </button>
          </div>
          {state && (
            <p className="mono small">
              SQLite tuples (rev {state.primaryRev}): {state.tuples.join(", ") || "none"}
            </p>
          )}
          {log.length > 0 && (
            <ul className="history">
              {log.map((e, i) => (
                <li key={i} className="small">
                  <span className="mono">t={e.r.t}</span>{" "}
                  {e.kind === "check" ? (
                    <>
                      <span className={e.r.correct ? "txt-ok" : "txt-bad"}>{e.r.correct ? "✓" : "✕"}</span> {e.r.subject} view {e.r.object} → {e.r.decision} via{" "}
                      {e.r.servedBy} ({fmtMs(e.r.latencyMs)} ms)
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

      <section className="card" aria-labelledby="why-h">
        <h2 id="why-h">3 · Why invalidation can't promise freshness, and how revision-aware does</h2>
        <RaceTimeline rows={rows} />
        <div className="two-col">
          <div>
            <h3>Invalidation: mitigated, never guaranteed</h3>
            <ul>
              <li>
                The SQLite commit and the cache update are <strong>two separate events</strong>. No transaction spans the database and the cache, so there is always a
                gap between them.
              </li>
              <li>Network delay, queueing, retries and GC pauses stretch the gap. A lost message stretches it to the full TTL.</li>
              <li>With several cache nodes, each has its own gap. Invalidating before the commit doesn't help either: a concurrent read can refill the old value.</li>
              <li>
                You can make the window smaller, but in a distributed system you can't make it zero. Any check that lands inside it gets the old answer.
              </li>
            </ul>
          </div>
          <div>
            <h3>Revision-aware: freshness decided on the read path</h3>
            <pre className="code">{`required = doc.revisionToken     // bumped in the same SQLite tx
entry    = cache.get(user, doc)

if entry and entry.rev >= required:
    return entry.decision        // fast path: fresh enough

decision = sqlite.check(user, doc)   // fallback: source of truth
cache.put(user, doc, decision, rev = sqlite.rev)
return decision`}</pre>
            <p className="small">
              The request carries the revision it needs, so correctness no longer depends on <em>when</em> a message arrives. The cost is the same SQLite
              round trip invalidation pays after a change, plus propagating tokens with requests.
            </p>
          </div>
        </div>
      </section>

      <TestsPanel />

      <footer className="muted small">Mock system with modelled latencies and simulated time, backed by an in-memory SQLite store per browser session.</footer>
    </div>
  );
}
