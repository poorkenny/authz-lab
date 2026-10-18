import { useEffect, useState } from "react";
import { api } from "../api";
import type { CheckResult, QueueMsg, State } from "../types";
import { fmtMs } from "./FlowDiagram";

const ALICE = "user:alice";
const DOC = "doc:42";
const who = (s: string) => s.replace("user:", "");
const isAlice = (m: QueueMsg) => m.subject === ALICE && m.object === DOC;

interface Read {
  n: number;
  r: CheckResult;
}

export function QueueSimulation() {
  const [state, setState] = useState<State | null>(null);
  const [reads, setReads] = useState<Read[]>([]);
  const [note, setNote] = useState("");
  const [flash, setFlash] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    api.sim.state().then(setState).catch((e: Error) => setError(e.message));
  }, []);

  async function act(f: () => Promise<string | void>) {
    setBusy(true);
    setError("");
    try {
      const msg = await f();
      if (msg) setNote(msg);
      setState(await api.sim.state());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const read = () =>
    act(async () => {
      const r = await api.sim.check(ALICE, DOC);
      setReads((l) => [{ n: (l[0]?.n ?? 0) + 1, r }, ...l].slice(0, 20));
      return r.reason;
    });
  const revoke = () => act(async () => (await api.sim.revoke(ALICE, DOC)).note);
  const regrant = () => act(async () => (await api.sim.grant(ALICE, DOC)).note);
  const others = (n: number) =>
    act(async () => {
      const rs = await api.sim.otherWrites(n);
      return `${rs.length} unrelated write${rs.length > 1 ? "s" : ""} committed; ${rs.length > 1 ? "their invalidations are" : "its invalidation is"} queued.`;
    });
  const deliver = () =>
    act(async () => {
      const d = await api.sim.deliver();
      if (d.message) {
        setFlash(d.message.id);
        setTimeout(() => setFlash(null), 1200);
      }
      return d.note;
    });
  const reset = () =>
    act(async () => {
      await api.sim.reset();
      setReads([]);
      return "Fresh world: Alice can view doc:42 and the queue is empty.";
    });
  const scenario = () =>
    act(async () => {
      await api.sim.reset();
      setReads([]);
      await api.sim.check(ALICE, DOC);
      await api.sim.otherWrites(3);
      const w = await api.sim.revoke(ALICE, DOC);
      return `Scenario loaded: Alice's ALLOW is cached, 3 unrelated invalidations are already queued, and Alice was just revoked. ${w.note}`;
    });

  const queued = state?.queued ?? [];
  const delivered = state?.delivered ?? [];
  const aliceEntry = state?.cache.find((e) => e.subject === ALICE && e.object === DOC);
  const aliceStale = aliceEntry ? aliceEntry.rev < (state?.docRev[DOC] ?? 0) : false;
  const aliceCanView = state?.tuples.includes(`${DOC}#viewer@${ALICE}`) ?? false;
  const stale = reads.filter((x) => !x.r.correct).length;

  return (
    <section className="card" aria-labelledby="sim-h">
      <h2 id="sim-h">3 · Simulation: read-after-write inconsistency in the invalidation queue</h2>
      <p>
        Invalidation events wait in the queue until <strong>you</strong> deliver them, one at a time, in order. Read as Alice at any point to see what the
        cache answers.
      </p>

      <div className="sim-controls">
        <button className="primary-btn" disabled={busy} onClick={scenario}>
          Load scenario: Alice's revoke behind 3 queued events
        </button>
        <span className="muted small">or build your own:</span>
      </div>
      <div className="sim-controls">
        <button disabled={busy} onClick={read}>
          Alice reads doc:42
        </button>
        <button disabled={busy || !aliceCanView} onClick={revoke}>
          Admin revokes Alice
        </button>
        <button disabled={busy || aliceCanView} onClick={regrant}>
          Admin re-grants Alice
        </button>
        <button disabled={busy} onClick={() => others(1)}>
          +1 other write
        </button>
        <button disabled={busy} onClick={() => others(3)}>
          +3 other writes
        </button>
        <button className="deliver-btn" disabled={busy || queued.length === 0} onClick={deliver}>
          Deliver next invalidation ▶
        </button>
        <button className="link" disabled={busy} onClick={reset}>
          Reset
        </button>
      </div>
      {error && <p className="txt-bad">{error}</p>}

      <div className="pipeline" aria-label="SQLite, invalidation queue and cache">
        <div className="stage-box">
          <div className="stage-title">SQLite (source of truth)</div>
          <div className="mono small">rev {state?.primaryRev ?? 0}</div>
          <div className={aliceCanView ? "txt-ok" : "txt-bad"}>
            Alice {aliceCanView ? "can" : "cannot"} view doc:42
          </div>
        </div>
        <div className="arrow" aria-hidden="true">
          →
        </div>
        <div className="stage-box queue-lane">
          <div className="stage-title">
            Invalidation queue <span className="muted">· {queued.length} queued · next delivery on the right</span>
          </div>
          <ol className="queue-cards">
            {queued.length === 0 && <li className="muted small empty">empty</li>}
            {[...queued].reverse().map((m, i) => (
              <li key={m.id} className={`qcard ${isAlice(m) ? "alice" : ""} ${i === queued.length - 1 ? "head" : ""}`}>
                <span className="mono">#{m.id}</span>
                <span>
                  {who(m.subject)}→{m.object}
                </span>
                <span className="muted small">
                  {m.op === "delete" ? "revoke" : "grant"} · rev {m.rev}
                </span>
                {i === queued.length - 1 && <span className="next-tag">next</span>}
              </li>
            ))}
          </ol>
        </div>
        <div className="arrow" aria-hidden="true">
          →
        </div>
        <div className={`stage-box cache-box ${aliceStale ? "stale" : ""}`}>
          <div className="stage-title">Permission cache</div>
          {aliceEntry ? (
            <div className={aliceStale ? "txt-bad" : "txt-ok"}>
              alice→doc:42: <strong>{aliceEntry.decision}</strong> @rev {aliceEntry.rev} {aliceStale ? "⚠ stale" : "✓ fresh"}
            </div>
          ) : (
            <div className="muted">no entry for alice→doc:42</div>
          )}
          <div className="delivered">
            <span className="muted small">delivered:</span>{" "}
            {delivered.length === 0 && <span className="muted small">none yet</span>}
            {delivered.map((m) => (
              <span key={m.id} className={`dchip ${isAlice(m) ? "alice" : ""} ${flash === m.id ? "flash" : ""}`}>
                #{m.id}
              </span>
            ))}
          </div>
        </div>
      </div>

      {note && (
        <p className="sim-note" aria-live="polite">
          {note}
        </p>
      )}

      <div className="row spread">
        <h3>Alice's reads</h3>
        <span className="small">
          <span className="txt-bad">✕ {stale} stale</span> · <span className="txt-ok">✓ {reads.length - stale} fresh</span>
        </span>
      </div>
      {reads.length === 0 ? (
        <p className="muted">No reads yet. Click “Alice reads doc:42”.</p>
      ) : (
        <div className="table-scroll">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">read</th>
                <th scope="col">cache answered</th>
                <th scope="col">SQLite says</th>
                <th scope="col">Alice's invalidation at that moment</th>
                <th scope="col">served by</th>
                <th scope="col" className="num">latency</th>
              </tr>
            </thead>
            <tbody>
              {reads.map(({ n, r }) => (
                <tr key={n} className={r.correct ? "" : "k-stale"}>
                  <td className="mono">#{n}</td>
                  <td className={r.correct ? "txt-ok" : "txt-bad"}>
                    {r.correct ? "✓" : "✕"} {r.decision}
                  </td>
                  <td className="mono">{r.expected}</td>
                  <td>
                    {r.pending
                      ? `#${r.pending.id} queued at position ${r.pendingPos} of ${r.queueDepth}${r.pendingPos! > 1 ? ` (${r.pendingPos! - 1} ahead of it)` : ""}`
                      : "none queued"}
                  </td>
                  <td>{r.servedBy === "primary" ? "SQLite" : "cache"}</td>
                  <td className="mono num">{fmtMs(r.latencyMs)} ms</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
