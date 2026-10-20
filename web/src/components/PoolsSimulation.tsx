import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import type { PoolCheckResult, PoolEvent, PoolMode, PoolRouting, PoolsState } from "../types";

const DOC = "doc:42";
const CAROL = "user:carol";
const POOLS = ["A", "B"];
const who = (s: string) => s.replace("user:", "");

interface Read {
  n: number;
  r: PoolCheckResult;
}

// Events a scenario remembers between its steps.
interface Ctx {
  grant?: PoolEvent;
  revoke?: PoolEvent;
}

interface Step {
  title: string;
  hint: string;
  run: (c: Ctx) => Promise<string>;
}

type ScenarioId = "order" | "race";

// The cache strategies for applying an invalidation event: id, label, description.
const MODES: [PoolMode, string, string][] = [
  ["retire", "Retire the entry", "Delete the cached entry; the next read goes to SQLite."],
  ["refresh", "Refresh decision + rev", "Write the event's decision and revision into the cache, whatever is there."],
  ["fenced", "Refresh with revision fence", "Write it only if its revision is ≥ the cached one; an older event is discarded."],
];

// PoolsSimulation (FEATURE_MULTI_POOL_INVALIDATION): several service
// instances, each publishing invalidation events to its own pool. Events for
// one entry can be applied out of order across pools, or race when a worker
// stalls between picking an event and writing it.
export function PoolsSimulation() {
  const [state, setState] = useState<PoolsState | null>(null);
  const [reads, setReads] = useState<Read[]>([]);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [subject, setSubject] = useState(CAROL);
  const [instance, setInstance] = useState(0);
  const [scenario, setScenario] = useState<ScenarioId | null>(null);
  const [done, setDone] = useState(0);
  const ctx = useRef<Ctx>({});

  useEffect(() => {
    api.pools
      .state()
      .then(setState)
      .catch((e: Error) => setError(e.message));
  }, []);

  async function act(f: () => Promise<string | void>) {
    setBusy(true);
    setError("");
    try {
      const msg = await f();
      if (msg) setNote(msg);
      setState(await api.pools.state());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const read = async (s: string) => {
    const r = await api.pools.check(s, DOC);
    setReads((l) => [{ n: (l[0]?.n ?? 0) + 1, r }, ...l].slice(0, 20));
    return r.reason;
  };
  // deliverUntil delivers a pool until the given event has been applied.
  const deliverUntil = async (e: PoolEvent) => {
    const notes: string[] = [];
    for (let i = 0; i < 40; i++) {
      const d = await api.pools.deliver(e.pool);
      if (!d.delivered) break;
      notes.push(d.note);
      if (d.apply?.event.id === e.id) break;
    }
    return notes.join(" ");
  };
  // drain lets every worker finish: stalled events first, then the queues.
  const drain = async () => {
    const notes: string[] = [];
    for (let i = 0; i < 80; i++) {
      const s = await api.pools.state();
      const busyPools = POOLS.map((_, p) => p).filter((p) => s.inflight[p] || s.queues[p].length > 0);
      if (busyPools.length === 0) break;
      for (const p of busyPools) {
        const d = await api.pools.deliver(p);
        if (d.apply?.event.subject === CAROL || d.note.startsWith("Worker")) notes.push(d.note);
      }
    }
    return notes.join(" ") || "Every pool is empty.";
  };

  const SCENARIOS: Record<ScenarioId, { title: string; steps: Step[] }> = {
    order: {
      title: "Out-of-order delivery across pools",
      steps: [
        {
          title: "Instance A is busy: 2 unrelated writes go through it",
          hint: "Their 2 events queue up first.",
          run: async () => {
            await api.pools.otherWrites(2, 0);
            return "2 unrelated writes went through instance A; their events wait on its pool.";
          },
        },
        {
          title: "Admin grants Carol through instance A",
          hint: "Its event (ALLOW) queues behind the backlog.",
          run: async (c) => {
            const w = await api.pools.grant(CAROL, DOC, 0);
            c.grant = w.event;
            return w.note;
          },
        },
        {
          title: "A second later, admin revokes Carol through instance B",
          hint: "Its event (DENY, newer revision) lands on an idle pool.",
          run: async (c) => {
            const w = await api.pools.revoke(CAROL, DOC, 1);
            c.revoke = w.event;
            return w.note;
          },
        },
        { title: "Deliver Carol's revoke", hint: "Its pool delivers it, with anything queued ahead of it.", run: (c) => deliverUntil(c.revoke!) },
        { title: "Carol reads doc:42", hint: "Right after the revoke was applied.", run: () => read(CAROL) },
        { title: "Every pool catches up", hint: "The older grant event arrives last.", run: drain },
        { title: "Carol reads doc:42 again", hint: "Every event has been applied. Is the cache right?", run: () => read(CAROL) },
      ],
    },
    race: {
      title: "Two workers race on the same entry",
      steps: [
        {
          title: "Admin grants Carol through instance A",
          hint: "Its event (ALLOW) is queued.",
          run: async (c) => {
            const w = await api.pools.grant(CAROL, DOC, 0);
            c.grant = w.event;
            return w.note;
          },
        },
        {
          title: "A worker picks up the grant and stalls",
          hint: "A GC pause or slow network between reading the event and writing the cache.",
          run: async (c) => (await api.pools.pick(c.grant!.pool)).note,
        },
        {
          title: "Admin revokes Carol through instance B",
          hint: "Its event (DENY, newer revision) is queued.",
          run: async (c) => {
            const w = await api.pools.revoke(CAROL, DOC, 1);
            c.revoke = w.event;
            return w.note;
          },
        },
        {
          title: "Deliver Carol's revoke",
          hint: "The events were picked up in commit order, but this one is written first.",
          run: (c) => deliverUntil(c.revoke!),
        },
        { title: "The stalled worker wakes up", hint: "It finishes the write it started before the revoke.", run: drain },
        { title: "Carol reads doc:42", hint: "Every event has been applied. Is the cache right?", run: () => read(CAROL) },
      ],
    },
  };

  const reconfigure = (mode: PoolMode, routing: PoolRouting) =>
    act(async () => {
      await api.pools.reset(mode, routing);
      setReads([]);
      setScenario(null);
      return "Fresh world with the new settings: Alice can view doc:42, Carol cannot, and the pools are empty.";
    });
  const start = (id: ScenarioId) =>
    act(async () => {
      const s = state!;
      await api.pools.reset(s.mode, s.routing);
      setReads([]);
      ctx.current = {};
      setScenario(id);
      setDone(0);
      return `Scenario loaded: ${SCENARIOS[id].title.toLowerCase()}. Run the steps in order.`;
    });
  const runStep = (i: number) =>
    act(async () => {
      const msg = await SCENARIOS[scenario!].steps[i].run(ctx.current);
      setDone(i + 1);
      return msg;
    });

  if (!state) return error ? <p className="txt-bad">{error}</p> : <p className="muted">Loading…</p>;

  const mode = state.mode;
  const canView = (s: string) => state.tuples.includes(`${DOC}#viewer@${s}`);
  const pendingFor = (s: string) =>
    state.queues.flat().filter((e) => e.subject === s && e.object === DOC).length + state.inflight.filter((e) => e?.subject === s && e.object === DOC).length;
  const steps = scenario ? SCENARIOS[scenario].steps : [];
  const finished = scenario !== null && done === steps.length;
  const last = reads[0]?.r;

  return (
    <div className="pools">
      <p>
        In production the Check API runs as several <strong>service instances</strong>. Each instance publishes the invalidation events of the writes it handles
        to <strong>its own pool</strong>, drained by its own worker. Events in one pool arrive in order, but nothing orders them across pools, so two events for
        the same entry can be applied in the opposite order to their commits.
      </p>
      <div className="principle" role="note">
        <strong>When is that a race?</strong> If the worker <em>retires</em> the cached entry (deletes it), order doesn't matter: deletes commute, and the next
        read goes to SQLite. If it <em>refreshes</em> the entry (writes the event's decision and version into the cache), a late, older event overwrites a newer
        entry, and since no further event is coming, a revoked user keeps access until the TTL ({state.ttlSec} s). A <em>revision fence</em> makes the refresh a
        compare-and-set on the entry's revision: an event older than the cached revision is discarded, so an old grant can never overwrite a newer revocation.
      </div>

      <div className="pool-config">
        <div className="cfg">
          <span className="small muted">Cache strategy: applying an event</span>
          <div className="segmented" role="radiogroup" aria-label="Cache strategy">
            {MODES.map(([id, label]) => (
              <button
                key={id}
                role="radio"
                aria-checked={mode === id}
                className={mode === id ? "on" : ""}
                disabled={busy}
                onClick={() => reconfigure(id, state.routing)}
              >
                {label}
              </button>
            ))}
          </div>
          <span className="small muted">{MODES.find(([id]) => id === mode)![2]}</span>
        </div>
        <div className="cfg">
          <span className="small muted">Event routing</span>
          <div className="segmented" role="radiogroup" aria-label="Event routing">
            <button
              role="radio"
              aria-checked={state.routing === "instance"}
              className={state.routing === "instance" ? "on" : ""}
              disabled={busy}
              onClick={() => reconfigure(mode, "instance")}
            >
              Each instance's own pool
            </button>
            <button
              role="radio"
              aria-checked={state.routing === "document"}
              className={state.routing === "document" ? "on" : ""}
              disabled={busy}
              onClick={() => reconfigure(mode, "document")}
            >
              Partitioned by document
            </button>
          </div>
        </div>
      </div>
      <p className="small muted">Changing a setting starts a fresh world.</p>

      <div className="sim-controls">
        <button className="primary-btn" disabled={busy} onClick={() => start("order")}>
          Scenario: out-of-order delivery
        </button>
        <button className="primary-btn" disabled={busy} onClick={() => start("race")}>
          Scenario: two workers race
        </button>
      </div>

      {scenario && (
        <ol className="pool-steps">
          {steps.map((s, i) => (
            <li key={s.title} className={i < done ? "done" : i === done ? "next" : ""}>
              <button disabled={busy || i !== done} onClick={() => runStep(i)} className={i === done ? "primary-btn" : ""}>
                <span className="step-n">{i < done ? "✓" : i + 1}</span> {s.title}
              </button>
              <span className="small muted">{s.hint}</span>
            </li>
          ))}
        </ol>
      )}
      {finished && last && <Outcome r={last} state={state} />}

      <details className="pool-free">
        <summary className="small">Build your own sequence</summary>
        <div className="sim-controls">
          <label className="small">
            User{" "}
            <select value={subject} onChange={(e) => setSubject(e.target.value)}>
              <option value="user:alice">Alice</option>
              <option value={CAROL}>Carol</option>
            </select>
          </label>
          <label className="small">
            through instance{" "}
            <select value={instance} onChange={(e) => setInstance(Number(e.target.value))}>
              {POOLS.map((p, i) => (
                <option key={p} value={i}>
                  {p}
                </option>
              ))}
            </select>
          </label>
          <button disabled={busy || canView(subject)} onClick={() => act(async () => (await api.pools.grant(subject, DOC, instance)).note)}>
            Grant {who(subject)}
          </button>
          <button disabled={busy || !canView(subject)} onClick={() => act(async () => (await api.pools.revoke(subject, DOC, instance)).note)}>
            Revoke {who(subject)}
          </button>
          <button
            disabled={busy}
            onClick={() => act(async () => `${(await api.pools.otherWrites(2, instance)).length} unrelated writes went through instance ${POOLS[instance]}.`)}
          >
            +2 other writes
          </button>
          <button disabled={busy} onClick={() => act(() => read(subject))}>
            {who(subject)} reads doc:42
          </button>
        </div>
      </details>
      {error && <p className="txt-bad">{error}</p>}

      <div className="pipeline pools-pipeline" aria-label="SQLite, invalidation pools and cache">
        <div className="stage-box">
          <div className="stage-title">SQLite (source of truth)</div>
          <div className="mono small">rev {state.primaryRev}</div>
          {["user:alice", CAROL].map((s) => (
            <div key={s} className={canView(s) ? "txt-ok" : "txt-bad"}>
              {who(s)} {canView(s) ? "can" : "cannot"} view doc:42
            </div>
          ))}
        </div>
        <div className="arrow" aria-hidden="true">
          →
        </div>
        <div className="pool-lanes">
          {POOLS.map((name, p) => {
            const q = state.queues[p];
            const stalled = state.inflight[p];
            return (
              <div key={name} className={`stage-box queue-lane ${stalled ? "stalled" : ""}`}>
                <div className="stage-title">
                  Pool {name}{" "}
                  <span className="muted">
                    · instance {name}'s worker · {q.length} queued
                  </span>
                  {state.routing === "document" && state.docPool === p && <span className="muted"> · all doc:42 events</span>}
                </div>
                <ol className="queue-cards">
                  {q.length === 0 && !stalled && <li className="muted small empty">empty</li>}
                  {[...q].reverse().map((e, i) => (
                    <EventCard key={e.id} e={e} head={i === q.length - 1 && !stalled} />
                  ))}
                  {stalled && <EventCard e={stalled} stalled />}
                </ol>
                <div className="sim-controls tight">
                  <button
                    className="deliver-btn"
                    disabled={busy || (!stalled && q.length === 0)}
                    onClick={() => act(async () => (await api.pools.deliver(p)).note)}
                  >
                    {stalled ? `Resume worker ${name} ▶` : `Deliver next ▶`}
                  </button>
                  <button disabled={busy || !!stalled || q.length === 0} onClick={() => act(async () => (await api.pools.pick(p)).note)}>
                    Pick up and stall ⏸
                  </button>
                </div>
              </div>
            );
          })}
        </div>
        <div className="arrow" aria-hidden="true">
          →
        </div>
        <div className="stage-box">
          <div className="stage-title">Permission cache</div>
          {["user:alice", CAROL].map((s) => {
            const e = state.cache.find((c) => c.subject === s && c.object === DOC);
            if (!e)
              return (
                <div key={s} className="muted">
                  no entry for {who(s)}→doc:42
                </div>
              );
            const stale = e.decision !== (canView(s) ? "ALLOW" : "DENY");
            const stuck = stale && pendingFor(s) === 0;
            return (
              <div key={s} className={stale ? "txt-bad" : "txt-ok"}>
                {who(s)}→doc:42: <strong>{e.decision}</strong> @rev {e.rev} {stuck ? "⚠ stale, stuck until TTL" : stale ? "⚠ stale" : "✓ fresh"}
              </div>
            );
          })}
          {state.history.length > 0 && (
            <ul className="applies small">
              {state.history.slice(0, 5).map((a, i) => (
                <li key={i} className={a.stale ? "txt-bad" : a.result === "discarded" ? "txt-ok" : "muted"}>
                  #{a.event.id} {who(a.event.subject)} {a.event.decision}@{a.event.rev} →{" "}
                  {a.result === "discarded" ? "discarded by the revision fence" : a.result}
                  {a.stale && " (overwrote a newer rev)"}
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>

      {note && (
        <p className="sim-note" aria-live="polite">
          {note}
        </p>
      )}

      <h3>Reads</h3>
      {reads.length === 0 ? (
        <p className="muted">No reads yet.</p>
      ) : (
        <div className="table-scroll">
          <table className="data">
            <thead>
              <tr>
                <th scope="col">read</th>
                <th scope="col">user</th>
                <th scope="col">cache answered</th>
                <th scope="col">SQLite says</th>
                <th scope="col">events still pending</th>
                <th scope="col">served by</th>
              </tr>
            </thead>
            <tbody>
              {reads.map(({ n, r }) => (
                <tr key={n} className={r.correct ? "" : "k-stale"}>
                  <td className="mono">#{n}</td>
                  <td>{who(r.subject)}</td>
                  <td className={r.correct ? "txt-ok" : "txt-bad"}>
                    {r.correct ? "✓" : "✕"} {r.decision} <span className="muted small">@rev {r.cacheRev}</span>
                  </td>
                  <td className="mono">
                    {r.expected} <span className="muted small">@rev {r.primaryRev}</span>
                  </td>
                  <td>{r.pending > 0 ? r.pending : r.correct ? "none" : <strong className="txt-bad">none: stuck until TTL</strong>}</td>
                  <td>{r.servedBy === "primary" ? "SQLite" : "cache"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function EventCard({ e, head, stalled }: { e: PoolEvent; head?: boolean; stalled?: boolean }) {
  return (
    <li className={`qcard ${e.subject === CAROL ? "alice" : ""} ${head ? "head" : ""} ${stalled ? "stalled" : ""}`}>
      <span className="mono">#{e.id}</span>
      <span>
        {who(e.subject)}→{e.object}
      </span>
      <span className="muted small">
        {e.decision} · rev {e.rev}
      </span>
      {head && <span className="next-tag">next</span>}
      {stalled && <span className="next-tag stall-tag">⏸ stalled</span>}
    </li>
  );
}

// Outcome explains the scenario's last read under the current settings.
function Outcome({ r, state }: { r: PoolCheckResult; state: PoolsState }) {
  if (!r.correct) {
    return (
      <div className="principle outcome bad" role="status">
        <strong>✕ Race: Carol was revoked but the cache still says {r.decision}</strong> (rev {r.cacheRev}, SQLite is at rev {r.primaryRev}). Every event has
        been applied, so nothing will correct it until the TTL ({state.ttlSec} s). Switch to <em>Refresh with revision fence</em>, <em>Retire the entry</em> or{" "}
        <em>partitioned by document</em>, and run the scenario again.
      </div>
    );
  }
  const why =
    state.mode === "retire"
      ? "the worker retires entries, and deletes give the same result in any order."
      : state.mode === "fenced"
        ? "the revision fence discarded the older event instead of letting it overwrite the newer revocation."
        : state.routing === "document"
          ? "all of doc:42's events go to one pool, so one worker applies them in commit order."
          : "the events did not overtake each other this time.";
  return (
    <div className="principle outcome good" role="status">
      <strong>✓ The cache agrees with SQLite ({r.decision})</strong>: {why}
      {state.mode === "refresh" && state.routing === "document" && " The cost: one slow pool delays every event of its documents."}
    </div>
  );
}
