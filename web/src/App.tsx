import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import type { CheckResult, CompareRow, DeliverResult, Features, Hop, State, Strategy, WriteResult } from "./types";
import { FlowDiagram, fmtMs, hopMs, shortLabel } from "./components/FlowDiagram";
import { QueueSimulation } from "./components/QueueSimulation";
import { TestsPanel } from "./components/TestsPanel";
import { STRATEGIES, byId } from "./strategies";

type Entry = { kind: "check"; r: CheckResult } | { kind: "write"; r: WriteResult } | { kind: "deliver"; r: DeliverResult };

interface Actions {
  check: (subject: string, object: string) => Promise<void>;
  write: (op: "grant" | "revoke", subject: string, object: string) => Promise<void>;
  deliver: () => Promise<void>;
}

type StepDef = { title: string; hint: string; run: (a: Actions) => Promise<void> };

// With controlled cache delivery the story pauses so the reviewer delivers the
// invalidation event; without it, events are delivered right after the commit.
const CONTROLLED_STEPS: StepDef[] = [
  { title: "Alice opens doc:42", hint: "Cache miss → SQLite → ALLOW is cached.", run: (a) => a.check("user:alice", "doc:42") },
  { title: "Admin revokes Alice", hint: "SQLite commits the revoke. Invalidation strategy: an event is queued.", run: (a) => a.write("revoke", "user:alice", "doc:42") },
  { title: "Alice reads before delivery", hint: "The invalidation event is still in the queue.", run: (a) => a.check("user:alice", "doc:42") },
  { title: "Deliver the invalidation event", hint: "Manual: delivers the head of the queue to the cache.", run: (a) => a.deliver() },
  { title: "Alice reads after delivery", hint: "Is the cache fresh now?", run: (a) => a.check("user:alice", "doc:42") },
];
const AUTO_STEPS: StepDef[] = [
  { title: "Alice opens doc:42", hint: "Cache miss → SQLite → ALLOW is cached.", run: (a) => a.check("user:alice", "doc:42") },
  { title: "Admin revokes Alice", hint: "SQLite commits the revoke. Invalidation strategy: the event is delivered right away.", run: (a) => a.write("revoke", "user:alice", "doc:42") },
  { title: "Alice reads again", hint: "Is the cache fresh?", run: (a) => a.check("user:alice", "doc:42") },
];

// Latency of each path through the system (sum of the modelled hops).
const PATHS = [
  { id: "hit", name: "Cache hit (TTL / invalidation)", hops: "request 0.5 + lookup 0.2 + response 0.5", ms: "1.2 ms" },
  { id: "ra-hit", name: "Revision-aware hit (entry fresh)", hops: "+ revision check 0.01", ms: "1.21 ms" },
  { id: "miss", name: "Cache miss → SQLite (incl. after a delivery)", hops: "+ SQLite query 15 + cache refresh 0.1", ms: "16.3 ms" },
  { id: "ra-fallback", name: "Revision-aware fallback (entry stale)", hops: "+ revision check + SQLite 15 + refresh", ms: "16.31 ms" },
  { id: "write", name: "Write: SQLite commit (+ enqueue)", hops: "commit 15 + enqueue 0.3; delivery is a separate, later event", ms: "15.3 ms" },
];

function pathOf(r: CheckResult): string {
  if (r.servedBy === "cache") return r.strategy === "revision_aware" ? "ra-hit" : "hit";
  return r.cacheRev > 0 && r.strategy === "revision_aware" ? "ra-fallback" : "miss";
}

export default function App() {
  const [state, setState] = useState<State | null>(null);
  const [flags, setFlags] = useState<Features>({ controlledCacheDelivery: true });
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
    api
      .features()
      .then(setFlags)
      .catch(() => {});
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

  const push = (e: Entry) => setLog((l) => [e, ...l].slice(0, 12));
  const actions: Actions = {
    check: (subject, object) =>
      act(async () => {
        const r = await api.check(subject, object);
        push({ kind: "check", r });
        setHops(r.hops);
        setActor(`Client (${subject.replace("user:", "")})`);
      }),
    write: (op, subject, object) =>
      act(async () => {
        const r = await api[op](subject, object);
        push({ kind: "write", r });
        setHops(r.hops);
      }),
    deliver: () =>
      act(async () => {
        const r = await api.deliver();
        push({ kind: "deliver", r });
        setHops(r.hops);
      }),
  };

  const controlled = flags.controlledCacheDelivery;
  const STEPS = controlled ? CONTROLLED_STEPS : AUTO_STEPS;

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

        <FlowDiagram hops={hops} state={state} actor={actor} onDeliver={controlled ? actions.deliver : undefined} busy={busy} />

        {lastEntry && (
          <div className="result-row">
            {lastEntry.kind === "check" ? (
              <div className={`result ${lastEntry.r.correct ? "ok" : "bad"}`} aria-live="polite">
                <div className="decision">
                  {lastEntry.r.subject.replace("user:", "")} → {lastEntry.r.object}: <strong>{lastEntry.r.decision}</strong>
                  <span className={`badge ${lastEntry.r.correct ? "ok" : "bad"}`}>
                    {lastEntry.r.correct ? "✓ fresh" : `✕ stale: SQLite says ${lastEntry.r.expected}`}
                  </span>
                  {lastEntry.r.pending && <span className="badge warn">invalidation #{lastEntry.r.pending.id} still queued</span>}
                </div>
                <p>{lastEntry.r.reason}</p>
                <p className="mono small">
                  served by {lastEntry.r.servedBy} · cache entry rev {lastEntry.r.cacheRev || "—"} · revision token {lastEntry.r.requiredRev || "not used"} · SQLite
                  rev {lastEntry.r.primaryRev} · {fmtMs(lastEntry.r.latencyMs)} ms
                </p>
              </div>
            ) : lastEntry.kind === "write" ? (
              <div className="result neutral" aria-live="polite">
                <div className="decision">
                  {lastEntry.r.op === "delete" ? "Revoked" : "Granted"} <span className="mono">{lastEntry.r.tuple}</span>
                </div>
                <p>{lastEntry.r.note}</p>
              </div>
            ) : (
              <div className="result neutral" aria-live="polite">
                <div className="decision">{lastEntry.r.delivered ? `Delivered invalidation #${lastEntry.r.message?.id}` : "Nothing to deliver"}</div>
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
                    <td className="mono num">{fmtMs(hops.reduce((a, h) => a + h.ms, 0))} ms</td>
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
                  <th scope="col">{controlled ? "read before delivery" : "read after revoke"}</th>
                  <th scope="col">{controlled ? "read after delivery" : "read again"}</th>
                  <th scope="col">verdict</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.strategy} className={r.strategy === strategy ? "current" : ""}>
                    <th scope="row">{byId[r.strategy].name}</th>
                    {[r.before, r.after].map((c, i) => (
                      <td key={i} className={c.correct ? "txt-ok" : "txt-bad"}>
                        {c.correct ? "✓" : "✕"} {c.decision} <span className="muted mono small">({c.servedBy}, {fmtMs(c.latencyMs)} ms)</span>
                      </td>
                    ))}
                    <td>{r.before.correct && r.after.correct ? "always fresh" : r.after.correct ? "stale until the event is delivered" : "stale until TTL"}</td>
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
              Check view
            </button>
            <button disabled={busy} onClick={() => actions.write("grant", user, doc)}>
              Grant viewer
            </button>
            <button disabled={busy} onClick={() => actions.write("revoke", user, doc)}>
              Revoke viewer
            </button>
            {controlled && (
              <button disabled={busy} onClick={actions.deliver}>
                Deliver next invalidation
              </button>
            )}
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
                  {e.kind === "check" ? (
                    <>
                      <span className={e.r.correct ? "txt-ok" : "txt-bad"}>{e.r.correct ? "✓" : "✕"}</span> {e.r.subject} view {e.r.object} → {e.r.decision} via{" "}
                      {e.r.servedBy} ({fmtMs(e.r.latencyMs)} ms)
                    </>
                  ) : e.kind === "write" ? (
                    <>
                      ✎ {e.r.op} {e.r.tuple} → rev {e.r.rev}
                      {e.r.message && <> · queued invalidation #{e.r.message.id}</>}
                    </>
                  ) : (
                    <>▶ {e.r.note}</>
                  )}
                </li>
              ))}
            </ul>
          )}
        </details>
      </section>

      {controlled && <QueueSimulation />}

      <TestsPanel section={controlled ? 4 : 3} />

      <footer className="muted small">Mock system with modelled latencies, backed by an in-memory SQLite store per browser session. {controlled ? "Invalidation events are delivered only when you click." : "Invalidation events are delivered automatically after each commit."}</footer>
    </div>
  );
}
