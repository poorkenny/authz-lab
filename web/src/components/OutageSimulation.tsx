import { useEffect, useState } from "react";
import { ApiError, api } from "../api";
import type { CheckOpts, CheckResult, Hop, State, Unavailable } from "../types";
import { FlowDiagram, fmtMs } from "./FlowDiagram";

const who = (s: string) => s.replace("user:", "");

// One response of the simulation: a 200 with a decision, or a 503.
interface Resp {
  n: number;
  request: string;
  primaryDown: boolean;
  r?: CheckResult;
  u?: Unavailable;
  // A failed write (503) has no check details.
  writeError?: string;
}

interface Ctx {
  check: (subject: string, object: string, opts: CheckOpts | undefined, request: string) => Promise<boolean>;
  open: () => Promise<boolean>;
  write: (op: "grant" | "revoke", subject: string, object: string) => Promise<boolean>;
  setPrimary: (down: boolean) => Promise<boolean>;
}

interface Step {
  title: string;
  hint: string;
  run: (c: Ctx) => Promise<boolean>;
  repeatable?: boolean;
  alts?: { label: string; run: (c: Ctx) => Promise<boolean> }[];
}

const steps = (held: number): Step[] => [
  { title: "Alice opens doc:42", hint: "SQLite answers ALLOW, cached as a snapshot at rev 3. Her client keeps the token ver#3.", run: (c) => c.open() },
  {
    title: "Admin grants Carol doc:42",
    hint: "doc:42 moves to ver#4. Alice's snapshot is still right, but older than doc:42's latest version.",
    run: (c) => c.write("grant", "user:carol", "doc:42"),
  },
  { title: "SQLite fails", hint: "Take the primary store down.", run: (c) => c.setPrimary(true) },
  {
    title: `Alice checks with her token ver#${held || "?"}`,
    hint: "The snapshot (rev 3) satisfies ver#3: answered from the cache, 200.",
    run: (c) => c.check("user:alice", "doc:42", { token: held }, `alice → doc:42, token ver#${held}`),
    repeatable: true,
  },
  {
    title: "Alice checks with the latest token",
    hint: "No snapshot satisfies ver#4: 503. Try the other requests that need SQLite too.",
    run: (c) => c.check("user:alice", "doc:42", undefined, "alice → doc:42, latest token"),
    repeatable: true,
    alts: [
      { label: "Carol checks (nothing cached)", run: (c) => c.check("user:carol", "doc:42", undefined, "carol → doc:42, latest token") },
      { label: "Fully consistent check", run: (c) => c.check("user:alice", "doc:42", { consistency: "fully_consistent" }, "alice → doc:42, fully consistent") },
      { label: "Admin revokes Carol", run: (c) => c.write("revoke", "user:carol", "doc:42") },
    ],
  },
  { title: "SQLite recovers", hint: "Bring the primary store back.", run: (c) => c.setPrimary(false) },
  {
    title: "Alice checks with the latest token again",
    hint: "Falls back to SQLite, answers 200 and refreshes the snapshot at rev 4.",
    run: (c) => c.check("user:alice", "doc:42", undefined, "alice → doc:42, latest token"),
  },
];

// OutageSimulation (FEATURE_SQLITE_FAILURE) shows failure handling when the
// primary store fails: a revision-aware check is still answered from a cached
// snapshot whose revision satisfies the request; otherwise the API returns 503.
export function OutageSimulation({ section }: { section: number }) {
  const [state, setState] = useState<State | null>(null);
  const [held, setHeld] = useState(0);
  const [step, setStep] = useState(0);
  const [resps, setResps] = useState<Resp[]>([]);
  const [hops, setHops] = useState<Hop[]>([]);
  // note is shown for actions without a response of their own (SQLite up/down, a committed write).
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [user, setUser] = useState("user:alice");
  const [token, setToken] = useState("latest");

  useEffect(() => {
    api.outage.reset().then(setState).catch((e: Error) => setError(e.message));
  }, []);

  async function act(f: () => Promise<void>): Promise<boolean> {
    setBusy(true);
    setError("");
    try {
      await f();
      return true;
    } catch (e) {
      setError((e as Error).message);
      return false;
    } finally {
      api.outage.state().then(setState).catch(() => {});
      setBusy(false);
    }
  }

  const push = (r: Omit<Resp, "n">) => {
    setResps((l) => [{ ...r, n: (l[0]?.n ?? 0) + 1 }, ...l].slice(0, 20));
    setNote("");
  };
  const down = state?.primaryDown ?? false;

  // A 503 is an expected outcome here, not an error: it is recorded as a response.
  const unavailable = (e: unknown): Unavailable | undefined => (e instanceof ApiError && e.status === 503 ? (e.body.unavailable as Unavailable) : undefined);

  const ctx: Ctx = {
    check: (subject, object, opts, request) =>
      act(async () => {
        try {
          const r = await api.outage.check(subject, object, opts);
          push({ request, primaryDown: r.primaryDown, r });
          setHops(r.hops);
        } catch (e) {
          const u = unavailable(e);
          if (!u) throw e;
          push({ request, primaryDown: true, u });
          setHops(u.hops);
        }
      }),
    open: () =>
      act(async () => {
        const r = await api.outage.check("user:alice", "doc:42");
        push({ request: "alice → doc:42, opens the doc", primaryDown: r.primaryDown, r });
        setHops(r.hops);
        setHeld(r.latestToken ?? 0);
      }),
    write: (op, subject, object) =>
      act(async () => {
        try {
          const w = await api.outage[op](subject, object);
          setHops(w.hops);
          setNote(w.note);
        } catch (e) {
          const u = unavailable(e);
          if (!u) throw e;
          push({ request: `admin ${op === "grant" ? "grants" : "revokes"} ${who(subject)} → ${object}`, primaryDown: true, u, writeError: u.message });
          setHops(u.hops);
        }
      }),
    setPrimary: (d) =>
      act(async () => {
        const p = await api.outage.setPrimary(d);
        setHops([]);
        setNote(p.note);
      }),
  };

  const STEPS = steps(held);
  async function runStep(run: Step["run"], i: number) {
    if (await run(ctx)) setStep((s) => Math.max(s, i + 1));
  }
  const reset = () =>
    void act(async () => {
      await api.outage.reset();
      setStep(0);
      setHeld(0);
      setResps([]);
      setHops([]);
      setNote("");
    });

  const last = resps[0];
  const served = resps.filter((x) => x.r).length;
  const failed = resps.length - served;

  return (
    <section className="card" aria-labelledby="outage-h">
      <h2 id="outage-h">{section} · Simulation: SQLite failure</h2>
      <p>
        A separate world with the revision-aware strategy. Take SQLite down and see which checks can still be answered. Each request carries a document
        version token: the client's own, or the document's latest.
      </p>
      <div className="principle" role="note">
        <strong>Failure handling rule.</strong> When SQLite fails, a check is answered from a cached snapshot only if the snapshot's revision is{" "}
        <em>equal to or newer than</em> the revision the request asks for. Otherwise the API returns <strong>503</strong>:{" "}
        <em>no snapshot satisfies the revision</em>. It never guesses with an older snapshot. A fully consistent check and a write always need SQLite.
      </div>

      <div className="sim-controls">
        <span className={`primary-status ${down ? "down" : "up"}`} role="status">
          SQLite: {down ? "✕ down" : "● up"}
        </span>
        <button className={down ? "" : "fail-btn"} disabled={busy} onClick={() => void ctx.setPrimary(!down)}>
          {down ? "Bring SQLite back" : "Simulate SQLite failure"}
        </button>
        <button className="link" disabled={busy} onClick={reset}>
          Reset
        </button>
      </div>

      <ol className="steps">
        {STEPS.map((s, i) => {
          const open = i === step || (s.repeatable && step === i + 1);
          return (
            <li key={i} className={i < step ? "done" : ""}>
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

      <FlowDiagram hops={hops} state={state} actor="Client" busy={busy} />

      {note ? (
        <p className="sim-note" aria-live="polite">
          {note}
        </p>
      ) : (
        last && <LastResponse resp={last} />
      )}

      <div className="sim-controls">
        <span className="muted small">Try your own request:</span>
        <select value={user} onChange={(e) => setUser(e.target.value)} aria-label="User">
          <option value="user:alice">Alice</option>
          <option value="user:bob">Bob (owner)</option>
          <option value="user:carol">Carol</option>
        </select>
        <span className="mono small">doc:42</span>
        <select value={token} onChange={(e) => setToken(e.target.value)} aria-label="Version token in the request">
          <option value="latest">token: latest (ver#{state?.docRev["doc:42"] ?? "?"})</option>
          {Array.from({ length: state?.primaryRev ?? 0 }, (_, i) => i + 1).map((v) => (
            <option key={v} value={String(v)}>
              token: ver#{v}
            </option>
          ))}
          <option value="strong">fully consistent (no token)</option>
        </select>
        <button
          disabled={busy}
          onClick={() =>
            void ctx.check(
              user,
              "doc:42",
              token === "latest" ? undefined : token === "strong" ? { consistency: "fully_consistent" } : { token: Number(token) },
              `${who(user)} → doc:42, ${token === "latest" ? "latest token" : token === "strong" ? "fully consistent" : `token ver#${token}`}`,
            )
          }
        >
          Check view
        </button>
      </div>

      <div className="row spread">
        <h3>Responses</h3>
        <span className="small">
          <span className="txt-ok">200 × {served}</span> · <span className="txt-bad">503 × {failed}</span>
        </span>
      </div>
      {resps.length === 0 ? (
        <p className="muted">No requests yet. Start with step 1.</p>
      ) : (
        <div className="table-scroll">
          <table className="data outage-table">
            <thead>
              <tr>
                <th scope="col">#</th>
                <th scope="col">request</th>
                <th scope="col">SQLite</th>
                <th scope="col">cached snapshot</th>
                <th scope="col">HTTP</th>
                <th scope="col">answer</th>
                <th scope="col" className="num">latency</th>
              </tr>
            </thead>
            <tbody>
              {resps.map((x) => (
                <tr key={x.n} className={x.u ? "k-stale" : ""}>
                  <td className="mono">#{x.n}</td>
                  <td>{x.request}</td>
                  <td>{x.primaryDown ? "down" : "up"}</td>
                  <td className="mono">{snapshot(x)}</td>
                  <td className={`mono ${x.u ? "txt-bad" : "txt-ok"}`}>{x.u ? "503" : "200"}</td>
                  <td>
                    {x.r ? (
                      <>
                        <strong>{x.r.decision}</strong> from {x.r.servedBy === "cache" ? "the snapshot" : "SQLite"}
                      </>
                    ) : x.writeError ? (
                      "write not committed"
                    ) : (
                      "no snapshot satisfies the revision"
                    )}
                  </td>
                  <td className="mono num">{fmtMs((x.r ?? x.u)!.latencyMs)} ms</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

// snapshot describes the cached snapshot a response was judged against.
function snapshot(x: Resp): string {
  if (x.writeError) return "—";
  if (x.u) {
    if (x.u.consistency === "fully_consistent") return "not used (fully consistent)";
    return x.u.snapshotRev ? `rev ${x.u.snapshotRev} < ver#${x.u.requiredRev}` : `none (requested ver#${x.u.requiredRev})`;
  }
  const r = x.r!;
  if (r.consistency === "fully_consistent") return "not used (fully consistent)";
  if (r.servedBy === "cache") return `rev ${r.cacheRev} ≥ ver#${r.requiredRev}`;
  return r.cacheRev ? `rev ${r.cacheRev} < ver#${r.requiredRev}, refreshed` : "none, now cached";
}

function LastResponse({ resp }: { resp: Resp }) {
  const { r, u } = resp;
  if (u) {
    return (
      <div className="result bad" aria-live="polite">
        <div className="decision">
          <span className="badge bad">503 Service Unavailable</span> {resp.request}
        </div>
        <p>{u.message}</p>
        {!resp.writeError && (
          <p className="mono small">
            requested {u.consistency === "fully_consistent" ? "the latest revision (fully consistent)" : `ver#${u.requiredRev}`} · cached snapshot{" "}
            {u.consistency === "fully_consistent" ? "not used" : u.snapshotRev ? `rev ${u.snapshotRev}` : "none"} · {fmtMs(u.latencyMs)} ms (incl. the
            SQLite timeout)
          </p>
        )}
      </div>
    );
  }
  if (!r) return null;
  return (
    <div className={`result ${r.correct ? "ok" : "bad"}`} aria-live="polite">
      <div className="decision">
        <span className="badge ok">200 OK</span> {who(r.subject)} → {r.object}: <strong>{r.decision}</strong>
        {r.primaryDown && <span className="badge warn">SQLite down: answered from the snapshot</span>}
        {r.primaryDown && r.contractMet && (
          <span className="badge ok">
            ✓ snapshot rev {r.evaluatedRev} ≥ requested ver#{r.requiredRev}
          </span>
        )}
      </div>
      <p>{r.reason}</p>
      <p className="mono small">
        served by {r.servedBy === "cache" ? "the cached snapshot" : "SQLite"} · snapshot rev {r.cacheRev || "—"} · requested{" "}
        {r.consistency === "fully_consistent" ? "the latest (fully consistent)" : `ver#${r.requiredRev}`} · {fmtMs(r.latencyMs)} ms
      </p>
    </div>
  );
}
