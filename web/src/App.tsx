import { useEffect, useRef, useState } from "react";
import { api } from "./api";
import type { CheckOpts, CheckResult, ClockResult, CompareRow, DeliverResult, Features, Hop, State, Strategy, WriteResult } from "./types";
import { CacheFreshness } from "./components/CacheFreshness";
import { FlowDiagram, fmtMs, hopMs, shortLabel } from "./components/FlowDiagram";
import { OutageSimulation } from "./components/OutageSimulation";
import { QueueSimulation } from "./components/QueueSimulation";
import { TestsPanel } from "./components/TestsPanel";
import { STRATEGIES, byId } from "./strategies";

type Entry = { kind: "check"; r: CheckResult } | { kind: "write"; r: WriteResult } | { kind: "deliver"; r: DeliverResult } | { kind: "clock"; r: ClockResult };

interface Actions {
  check: (subject: string, object: string, opts?: CheckOpts) => Promise<boolean>;
  // open is a check after which the client keeps the document's version token.
  open: (subject: string, object: string) => Promise<boolean>;
  write: (op: "grant" | "revoke", subject: string, object: string) => Promise<boolean>;
  deliver: () => Promise<boolean>;
  advance: (seconds: number) => Promise<boolean>;
}

type StepDef = {
  title: string;
  hint: string;
  run: (a: Actions) => Promise<boolean>;
  // A repeatable step stays clickable until the step after it is done.
  repeatable?: boolean;
  // Extra actions shown next to the step while it is clickable.
  alts?: { label: string; run: (a: Actions) => Promise<boolean> }[];
};

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

// Strategy stories (FEATURE_STRATEGY_STORIES): each strategy gets only the
// steps that make sense for it. TTL sends no invalidations, so instead of a
// delivery step the clock is fast-forwarded past the TTL.
const TTL_SEC = 300;
// With FEATURE_CLIENT_VERSION_TOKEN the revision-aware story shows that the
// strategy promises "at least as fresh as the token in the request", not "the
// latest": an old token can still read an old result.
interface StoryCtx {
  controlled: boolean;
  tokens: boolean;
  held: number; // version token Alice's client kept when it opened the doc
}
const STORIES: Record<Strategy, (c: StoryCtx) => StepDef[]> = {
  ttl: () => [
    { title: "Alice opens doc:42", hint: `Cache miss → SQLite → ALLOW is cached with a ${TTL_SEC} s TTL.`, run: (a) => a.check("user:alice", "doc:42") },
    { title: "Admin revokes Alice", hint: "SQLite commits the revoke. Nothing tells the cache.", run: (a) => a.write("revoke", "user:alice", "doc:42") },
    { title: "Alice reads within the TTL", hint: "The entry hasn't expired yet. Watch its age and time left below.", run: (a) => a.check("user:alice", "doc:42"), repeatable: true },
    { title: `Fast-forward ${TTL_SEC} s`, hint: "Skip the simulated clock to the end of the TTL.", run: (a) => a.advance(TTL_SEC) },
    { title: "Alice reads after the TTL", hint: "The entry expired, so SQLite answers.", run: (a) => a.check("user:alice", "doc:42") },
  ],
  invalidation: ({ controlled }) =>
    controlled
      ? [
          { title: "Alice opens doc:42", hint: "Cache miss → SQLite → ALLOW is cached.", run: (a) => a.check("user:alice", "doc:42") },
          { title: "Admin revokes Alice", hint: "SQLite commits the revoke; its invalidation event is queued.", run: (a) => a.write("revoke", "user:alice", "doc:42") },
          {
            title: "Check before delivery",
            hint: "Check as often as you like while the event is queued: each read shows what it got next to the happy path.",
            run: (a) => a.check("user:alice", "doc:42"),
            repeatable: true,
            alts: [{ label: "Bob reads doc:42", run: (a) => a.check("user:bob", "doc:42") }],
          },
          { title: "Deliver the invalidation event", hint: "Manual: delivers the head of the queue to the cache.", run: (a) => a.deliver() },
          { title: "Alice reads after delivery", hint: "The entry is gone, so SQLite answers.", run: (a) => a.check("user:alice", "doc:42") },
        ]
      : AUTO_STEPS,
  revision_aware: ({ tokens, held }) =>
    tokens
      ? [
          {
            title: "Alice opens doc:42",
            hint: "Cache miss → SQLite → ALLOW cached at rev 3. Her client keeps doc:42's version token with the document.",
            run: (a) => a.open("user:alice", "doc:42"),
          },
          {
            title: "Admin revokes Alice",
            hint: `doc:42 moves to a newer version; Alice's client still holds ver#${held || "?"}.`,
            run: (a) => a.write("revoke", "user:alice", "doc:42"),
          },
          {
            title: `Check with her old token ver#${held || "?"}`,
            hint: "The cached entry is not older than the token, so it is served: promise kept, but not the latest.",
            run: (a) => a.check("user:alice", "doc:42", { token: held }),
            repeatable: true,
          },
          { title: "Check with the latest token", hint: "The entry is older than doc:42's latest version: fallback to SQLite.", run: (a) => a.check("user:alice", "doc:42") },
          {
            title: "Fully consistent check",
            hint: "Skip the cache and ask SQLite: always the latest, always the slow path.",
            run: (a) => a.check("user:alice", "doc:42", { consistency: "fully_consistent" }),
          },
        ]
      : [
    { title: "Alice opens doc:42", hint: "Cache miss → SQLite → ALLOW is cached, stamped with its revision.", run: (a) => a.check("user:alice", "doc:42") },
    { title: "Admin revokes Alice", hint: "SQLite commits the revoke; doc:42's revision token moves forward.", run: (a) => a.write("revoke", "user:alice", "doc:42") },
    { title: "Alice reads after the revoke", hint: "The entry is older than the token: fallback to SQLite.", run: (a) => a.check("user:alice", "doc:42") },
    { title: "Alice reads again", hint: "The refreshed entry matches the token: a fast, correct hit.", run: (a) => a.check("user:alice", "doc:42") },
  ],
};

// Latency of each path through the system (sum of the modelled hops).
const PATHS = [
  { id: "hit", name: "Cache hit (TTL / invalidation)", hops: "request 0.5 + lookup 0.2 + response 0.5", ms: "1.2 ms" },
  { id: "ra-hit", name: "Revision-aware hit (entry fresh)", hops: "+ revision check 0.01", ms: "1.21 ms" },
  { id: "miss", name: "Cache miss → SQLite (incl. after a delivery)", hops: "+ SQLite query 15 + cache refresh 0.1", ms: "16.3 ms" },
  { id: "ra-fallback", name: "Revision-aware fallback (entry stale)", hops: "+ revision check + SQLite 15 + refresh", ms: "16.31 ms" },
  { id: "strong", name: "Fully consistent read (always SQLite)", hops: "request 0.5 + SQLite 15 + response 0.5; no cache", ms: "16.0 ms" },
  { id: "write", name: "Write: SQLite commit (+ enqueue)", hops: "commit 15 + enqueue 0.3; delivery is a separate, later event", ms: "15.3 ms" },
];

function pathOf(r: CheckResult): string {
  if (r.consistency === "fully_consistent") return "strong";
  if (r.servedBy === "cache") return r.strategy === "revision_aware" ? "ra-hit" : "hit";
  return r.cacheRev > 0 && r.strategy === "revision_aware" ? "ra-fallback" : "miss";
}

export default function App() {
  const [state, setState] = useState<State | null>(null);
  const [flags, setFlags] = useState<Features>({ controlledCacheDelivery: true, strategyStories: true, happyPathComparison: true, clientVersionToken: true, sqliteFailure: true });
  const [held, setHeld] = useState(0);
  const [reqToken, setReqToken] = useState("latest");
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
    // The session survives reloads but the story restarts at step 1, so start
    // from a fresh world (same strategy) to keep the two in sync.
    api.reset().then(setState).catch((e: Error) => setError(e.message));
    api
      .compare()
      .then((c) => setRows(c.rows))
      .catch(() => {});
  }, []);

  // act runs an API action and refreshes state; it resolves to false if the action failed.
  async function act(f: () => Promise<void>): Promise<boolean> {
    setBusy(true);
    setError("");
    try {
      await f();
      setState(await api.state());
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      setBusy(false);
    }
  }

  const push = (e: Entry) => setLog((l) => [e, ...l].slice(0, 12));
  const actions: Actions = {
    check: (subject, object, opts) =>
      act(async () => {
        const r = await api.check(subject, object, opts);
        push({ kind: "check", r });
        setHops(r.hops ?? []);
        setActor(`Client (${subject.replace("user:", "")})`);
      }),
    open: (subject, object) =>
      act(async () => {
        const r = await api.check(subject, object);
        push({ kind: "check", r });
        setHops(r.hops ?? []);
        setActor(`Client (${subject.replace("user:", "")})`);
        setHeld(r.latestToken ?? 0);
      }),
    write: (op, subject, object) =>
      act(async () => {
        const r = await api[op](subject, object);
        push({ kind: "write", r });
        setHops(r.hops ?? []);
      }),
    deliver: () =>
      act(async () => {
        const r = await api.deliver();
        push({ kind: "deliver", r });
        setHops(r.hops ?? []);
      }),
    advance: (seconds) =>
      act(async () => {
        const r = await api.advanceClock(seconds);
        push({ kind: "clock", r });
        setHops(r.hops ?? []);
      }),
  };

  const controlled = flags.controlledCacheDelivery;
  const stories = flags.strategyStories;
  const strategy = state?.strategy ?? "ttl";
  const tokens = flags.clientVersionToken && strategy === "revision_aware";
  const STEPS = stories ? STORIES[strategy]({ controlled, tokens, held }) : controlled ? CONTROLLED_STEPS : AUTO_STEPS;
  // Manual delivery only makes sense for the strategy that has a queue.
  const canDeliver = controlled && (!stories || strategy === "invalidation");

  async function runStep(run: StepDef["run"], i: number) {
    // Only advance when the step's action succeeded, so the story can't drift
    // out of sync with the server (e.g. revoking an already-revoked user).
    // Repeating a step never moves the story backwards.
    if (await run(actions)) setStep((s) => Math.max(s, i + 1));
  }

  const switchTo = (s: Strategy) =>
    void act(async () => {
      await api.setStrategy(s);
      setStep(0);
      setLog([]);
      setHops([]);
    });

  const lastEntry = log[0];
  const lastCheck = lastEntry?.kind === "check" ? lastEntry.r : undefined;

  return (
    <div className="app">
      <header>
        <p className="eyebrow">Distributed authorization</p>
        <h1>RevokeLab: a platform helps determine the permission refresh strategies in distributed authorization system</h1>
        <div className="goal" role="note">
          <strong>Goal of the platform:</strong> help security engineers <strong>choose the permission refresh strategy that meets the business needs</strong>.
          RevokeLab <strong>runs simulations based on your permission revocation needs</strong>: a revoked user reading again, invalidations stuck behind a
          queue backlog, clients sending old version tokens, a SQLite outage, and randomized workloads of grants and revokes. For each strategy (TTL cache,
          TTL + invalidation, revision-aware) it shows how long a revoked user can keep a <strong>stale ALLOW</strong>, what the strategy can and cannot
          guarantee, and what it costs in latency. You can then pick the strategy whose trade-off fits how fast access must be revoked.
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

        {tokens && (
          <div className="principle" role="note">
            <strong>Design principle of the revision-aware strategy.</strong> A check is evaluated at a version <em>at least as new as the token in the
            request</em>. It does <em>not</em> promise the latest version: a client that sends an old token can still get an old (but consistent) answer. Only a{" "}
            <strong>fully consistent</strong> read promises the latest, by skipping the cache and asking the single source of truth on every check. That is
            strong consistency without scalability: no cache offload, the full primary round trip every time.
          </div>
        )}

        <ol className="steps">
          {STEPS.map((s, i) => {
            const open = i === step || (s.repeatable && step === i + 1);
            return (
              <li key={s.title} className={i < step ? "done" : ""}>
                <button className={i === step ? "primary-btn" : ""} disabled={busy || !open} onClick={() => runStep(s.run, i)}>
                  <span className="step-n">{i < step ? "✓" : i + 1}</span> {s.title}
                  {s.repeatable && i < step && open && " (again)"}
                </button>
                {s.alts?.map((alt) => (
                  <button key={alt.label} className="alt-btn" disabled={busy || !open} onClick={() => runStep(alt.run, i)}>
                    {alt.label}
                  </button>
                ))}
                <span className="muted small">{s.hint}</span>
              </li>
            );
          })}
        </ol>
        {error && <p className="txt-bad">{error}</p>}

        <FlowDiagram hops={hops} state={state} actor={actor} onDeliver={canDeliver ? actions.deliver : undefined} busy={busy} />

        {stories && state?.enforceTTL && <CacheFreshness state={state} highlight={strategy === "ttl"} />}

        {lastEntry && (
          <div className="result-row">
            {lastEntry.kind === "check" ? (
              <div className={`result ${lastEntry.r.correct ? "ok" : lastEntry.r.tokenSource === "client" && lastEntry.r.contractMet ? "warn" : "bad"}`} aria-live="polite">
                <div className="decision">
                  {lastEntry.r.subject.replace("user:", "")} → {lastEntry.r.object}: <strong>{lastEntry.r.decision}</strong>
                  <span className={`badge ${lastEntry.r.correct ? "ok" : "bad"}`}>
                    {lastEntry.r.correct ? (lastEntry.r.consistency === "fully_consistent" ? "✓ latest" : "✓ fresh") : lastEntry.r.tokenSource === "client" ? `✕ not the latest: ${lastEntry.r.object} is at ver#${lastEntry.r.latestToken}, SQLite says ${lastEntry.r.expected}` : `✕ stale: SQLite says ${lastEntry.r.expected}`}
                  </span>
                  {flags.clientVersionToken && lastEntry.r.tokenSource === "client" && lastEntry.r.contractMet && (
                    <span className="badge ok">
                      ✓ promise kept: evaluated at rev {lastEntry.r.evaluatedRev} ≥ request token ver#{lastEntry.r.requiredRev}
                    </span>
                  )}
                  {lastEntry.r.consistency === "fully_consistent" && <span className="badge warn">fully consistent: cache skipped</span>}
                  {lastEntry.r.pending && <span className="badge warn">invalidation #{lastEntry.r.pending.id} still queued</span>}
                </div>
                <p>{lastEntry.r.reason}</p>
                <p className="mono small">
                  served by {lastEntry.r.servedBy} · cache entry rev {lastEntry.r.cacheRev || "—"}
                  {stories && lastEntry.r.servedBy === "cache" && <> (age {Math.floor(lastEntry.r.entryAgeSec)} s of {lastEntry.r.ttlSec} s TTL)</>} · doc version token{" "}
                  {lastEntry.r.tokenSource === "client"
                    ? `from the request ver#${lastEntry.r.requiredRev} (latest ${lastEntry.r.object} ver#${lastEntry.r.latestToken})`
                    : lastEntry.r.consistency === "fully_consistent"
                      ? "not used (fully consistent)"
                      : lastEntry.r.requiredRev
                        ? `${lastEntry.r.object} ver#${lastEntry.r.requiredRev}`
                        : "not used"}{" "}
                  · SQLite rev {lastEntry.r.primaryRev} · {fmtMs(lastEntry.r.latencyMs)} ms
                </p>
                {lastEntry.r.happyPath && <HappyPathCompare r={lastEntry.r} />}
              </div>
            ) : lastEntry.kind === "write" ? (
              <div className="result neutral" aria-live="polite">
                <div className="decision">
                  {lastEntry.r.op === "delete" ? "Revoked" : "Granted"} <span className="mono">{lastEntry.r.tuple}</span>
                </div>
                <p>{lastEntry.r.note}</p>
              </div>
            ) : lastEntry.kind === "deliver" ? (
              <div className="result neutral" aria-live="polite">
                <div className="decision">{lastEntry.r.delivered ? `Delivered invalidation #${lastEntry.r.message?.id}` : "Nothing to deliver"}</div>
                <p>{lastEntry.r.note}</p>
              </div>
            ) : (
              <div className="result neutral" aria-live="polite">
                <div className="decision">Fast-forwarded {lastEntry.r.advancedSec} s</div>
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
                {PATHS.filter((p) => p.id !== "strong" || flags.clientVersionToken).map((p) => {
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
                  <th scope="col">{stories ? "read after delivery / TTL expiry" : controlled ? "read after delivery" : "read again"}</th>
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
                    <td>{r.before.correct && r.after.correct ? "always fresh" : r.strategy === "ttl" || !r.after.correct ? "stale until TTL" : "stale until the event is delivered"}</td>
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
            {tokens && state && (
              <select value={reqToken} onChange={(e) => setReqToken(e.target.value)} aria-label="Version token in the request">
                <option value="latest">token: latest (doc's current version)</option>
                {Array.from({ length: state.primaryRev }, (_, i) => i + 1).map((v) => (
                  <option key={v} value={String(v)}>
                    token: ver#{v}
                  </option>
                ))}
                <option value="strong">fully consistent (no token)</option>
              </select>
            )}
            <button
              disabled={busy}
              onClick={() =>
                actions.check(user, doc, !tokens || reqToken === "latest" ? undefined : reqToken === "strong" ? { consistency: "fully_consistent" } : { token: Number(reqToken) })
              }
            >
              Check view
            </button>
            <button disabled={busy} onClick={() => actions.write("grant", user, doc)}>
              Grant viewer
            </button>
            <button disabled={busy} onClick={() => actions.write("revoke", user, doc)}>
              Revoke viewer
            </button>
            {canDeliver && (
              <button disabled={busy} onClick={actions.deliver}>
                Deliver next invalidation
              </button>
            )}
            {stories && (
              <button disabled={busy} onClick={() => actions.advance(60)}>
                Fast-forward 60 s
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
                  ) : e.kind === "deliver" ? (
                    <>▶ {e.r.note}</>
                  ) : (
                    <>⏩ {e.r.note}</>
                  )}
                </li>
              ))}
            </ul>
          )}
        </details>
      </section>

      {controlled && <QueueSimulation />}

      {flags.sqliteFailure && <OutageSimulation section={controlled ? 4 : 3} />}

      <TestsPanel section={3 + Number(controlled) + Number(flags.sqliteFailure)} />

      <footer className="muted small">Mock system with modelled latencies, backed by an in-memory SQLite store per browser session. {controlled ? "Invalidation events are delivered only when you click." : "Invalidation events are delivered automatically after each commit."}</footer>
    </div>
  );
}

// HappyPathCompare puts a read made while its invalidation is still queued
// next to the same read on the happy path (event delivered first).
function HappyPathCompare({ r }: { r: CheckResult }) {
  const hp = r.happyPath!;
  return (
    <div className="happy-compare">
      <div className={`happy-col ${r.correct ? "ok" : "bad"}`}>
        <div className="small muted">This read: invalidation #{r.pending?.id} still queued</div>
        <div className="decision">
          <strong>{r.decision}</strong>
          <span className={`badge ${r.correct ? "ok" : "bad"}`}>{r.correct ? "✓ correct" : "✕ incorrect"}</span>
        </div>
        <div className="mono small">
          served by {r.servedBy} · {fmtMs(r.latencyMs)} ms
        </div>
      </div>
      <div className="happy-col ok">
        <div className="small muted">Happy path: invalidation delivered before the read</div>
        <div className="decision">
          <strong>{hp.decision}</strong>
          <span className="badge ok">✓ correct</span>
        </div>
        <div className="mono small">
          served by SQLite · {fmtMs(hp.latencyMs)} ms
        </div>
      </div>
      <p className="small happy-note">{hp.note}</p>
    </div>
  );
}
