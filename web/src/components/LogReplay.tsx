import { useState } from "react";
import { ApiError, api } from "../api";
import type { ObservedStats, ReplayResult, ReplayStats } from "../types";
import { STRATEGIES } from "../strategies";

const MAX_BYTES = 2 << 20;

const fmtSec = (s: number) => (s === 0 ? "0 s" : s < 1 ? `${(s * 1000).toFixed(0)} ms` : `${s.toFixed(s < 10 ? 1 : 0)} s`);

// A column of the results table: one strategy, fully consistent reads, or the
// decisions recorded in the log itself.
interface Col {
  id: string;
  name: string;
  s: ReplayStats | null;
  recorded?: boolean;
}

// LogReplay (FEATURE_LOG_REPLAY) uploads a log of permission grants,
// revocations and checks and replays it against every strategy.
export function LogReplay() {
  const [log, setLog] = useState("");
  const [fileName, setFileName] = useState("");
  const [delay, setDelay] = useState(1000);
  const [result, setResult] = useState<ReplayResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  async function onFile(f: File | undefined) {
    setError("");
    if (!f) return;
    if (f.size > MAX_BYTES) {
      setError(`${f.name} is ${(f.size / 1048576).toFixed(1)} MB; the limit is 2 MB.`);
      return;
    }
    setLog(await f.text());
    setFileName(f.name);
    setResult(null);
  }

  async function sample() {
    setError("");
    try {
      const s = await api.sampleLog();
      setLog(s.log);
      setDelay(s.invalidationMs);
      setFileName("sample log");
      setResult(null);
    } catch (e) {
      setError((e as Error).message);
    }
  }

  async function run() {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      setResult(await api.replay(log, delay));
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) setNotice(e.message);
      else setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const cols: Col[] = result
    ? [
        ...STRATEGIES.map((s) => ({ id: s.id, name: s.name, s: result.byStrategy[s.id] })),
        { id: "strong", name: "Fully consistent reads", s: result.fullyConsistent },
        ...(result.observed ? [{ id: "observed", name: "Your system (decisions in the log)", s: null, recorded: true }] : []),
      ]
    : [];
  const o = result?.observed;
  const rows: [string, (s: ReplayStats) => string, (o: ObservedStats) => string, "errors"?][] = [
    ["checks", (s) => s.checks.toLocaleString(), (o) => o.checks.toLocaleString()],
    ["wrong decisions", (s) => s.wrong.toLocaleString(), (o) => o.wrong.toLocaleString(), "errors"],
    ["stale ALLOWs (after a revocation)", (s) => s.staleAllows.toLocaleString(), (o) => o.staleAllows.toLocaleString(), "errors"],
    ["longest access after a revocation", (s) => fmtSec(s.maxStaleSec), (o) => fmtSec(o.maxStaleSec), "errors"],
    ["false DENYs (after a grant)", (s) => s.falseDenies.toLocaleString(), (o) => o.falseDenies.toLocaleString(), "errors"],
    ["cache hit rate", (s) => `${s.hitRatePct.toFixed(1)}%`, () => "—"],
    ["checks sent to SQLite", (s) => `${s.sqliteReads.toLocaleString()} (${pct(s.sqliteReads, s.checks)})`, () => "—"],
    ["p50 check latency", (s) => `${s.p50Ms.toFixed(2)} ms`, () => "—"],
    ["p99 check latency", (s) => `${s.p99Ms.toFixed(2)} ms`, () => "—"],
  ];

  return (
    <div className="replay">
      <p>
        Upload a log of <strong>permission revocation events</strong> and <strong>permission check events</strong> from your system. RevokeLab replays it, in
        timestamp order, against each strategy and against fully consistent reads, and reports how long a revoked user kept access and what each strategy cost.
      </p>
      <details className="format">
        <summary>Log format</summary>
        <p className="small">
          JSON Lines or CSV, one event per line. <code>event</code> is <code>grant</code>, <code>revoke</code> or <code>check</code>. <code>ts</code> is an RFC
          3339 timestamp or a number of seconds. Bare IDs become <code>user:…</code> / <code>doc:…</code>. A check can carry the <code>decision</code> your
          system returned (ALLOW or DENY); RevokeLab then evaluates those decisions too. The replay starts from an empty store, so include a grant for every
          permission that exists before the first check. Up to 20,000 events and 2 MB.
        </p>
        <pre className="mono small">{`{"ts":"2026-10-18T09:00:00Z","event":"grant","subject":"user:alice","object":"doc:42"}
{"ts":"2026-10-18T09:01:00Z","event":"revoke","subject":"user:alice","object":"doc:42"}
{"ts":"2026-10-18T09:01:00.3Z","event":"check","subject":"user:alice","object":"doc:42","decision":"ALLOW"}

ts,event,subject,object,decision
0,grant,alice,42,
60,revoke,alice,42,
60.3,check,alice,42,ALLOW`}</pre>
        <p className="small">
          Cache entries expire after the TTL ({result?.ttlSec ?? 300} s), and each invalidation event is delivered the chosen delay after its commit.
        </p>
      </details>

      <div className="sim-controls">
        <label className="file-btn">
          <input type="file" accept=".jsonl,.json,.ndjson,.csv,.log,.txt" onChange={(e) => void onFile(e.target.files?.[0])} />
          Upload log file
        </label>
        <button disabled={busy} onClick={sample}>
          Load sample log
        </button>
        <label className="small">
          Invalidation delivery delay{" "}
          <input
            type="number"
            min={0}
            max={3600000}
            step={100}
            value={delay}
            onChange={(e) => setDelay(Math.max(0, Number(e.target.value)))}
            className="ms-input"
          />{" "}
          ms
        </label>
      </div>
      <textarea
        className="log-input mono"
        value={log}
        onChange={(e) => {
          setLog(e.target.value);
          setFileName("");
        }}
        placeholder="Upload a log file, load the sample, or paste events here."
        rows={8}
        spellCheck={false}
        aria-label="Log of permission events"
      />
      <div className="sim-controls">
        <button className="primary-btn" disabled={busy || !log.trim()} onClick={run}>
          {busy ? "Replaying…" : "Run benchmark on this log"}
        </button>
        <span className="muted small">
          {fileName && `${fileName} · `}
          {log
            ? `${log
                .split("\n")
                .filter((l) => l.trim())
                .length.toLocaleString()} lines`
            : "no log yet"}
        </span>
      </div>
      {notice && (
        <p className="sim-note" role="status">
          {notice}
        </p>
      )}
      {error && <p className="txt-bad">{error}</p>}

      {result && (
        <>
          <p className="muted small">
            Replayed {result.events.toLocaleString()} events over {fmtSec(result.spanSec)}: {result.grants.toLocaleString()} grants,{" "}
            {result.revokes.toLocaleString()} revocations, {result.checks.toLocaleString()} checks · invalidation delay {result.invalidationMs} ms · TTL{" "}
            {result.ttlSec} s · {result.durationMs.toFixed(0)} ms
            {result.skipped > 0 && <span className="txt-bad"> · {result.skipped} lines skipped</span>}
          </p>
          {result.issues.length > 0 && (
            <details className="issues">
              <summary className="small">Skipped lines ({result.skipped})</summary>
              <ul className="mono small">
                {result.issues.map((i) => (
                  <li key={i}>{i}</li>
                ))}
              </ul>
            </details>
          )}
          <Recommendation result={result} />
          <div className="table-scroll">
            <table className="data replay-table">
              <thead>
                <tr>
                  <th scope="col"></th>
                  {cols.map((c) => (
                    <th key={c.id} scope="col">
                      {c.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map(([label, f, fo, kind]) => (
                  <tr key={label}>
                    <th scope="row">{label}</th>
                    {cols.map((c) => {
                      const v = c.recorded ? fo(o!) : f(c.s!);
                      const zero = v === "0" || v === "0 s";
                      return (
                        <td key={c.id} className={`mono ${kind === "errors" ? (zero ? "txt-ok" : "txt-bad") : ""}`}>
                          {v}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

const listOf = (names: string[]) => (names.length < 2 ? names.join("") : `${names.slice(0, -1).join(", ")} and ${names[names.length - 1]}`);

const pct = (n: number, d: number) => (d ? `${((100 * n) / d).toFixed(1)}%` : "—");

// Recommendation summarises the trade-off on this log: which strategies never
// let a revoked user keep access, which of those loads SQLite least, and how
// long the others let access linger.
function Recommendation({ result }: { result: ReplayResult }) {
  if (result.checks === 0) return <p className="muted small">The log has no checks, so there is nothing to compare.</p>;
  const all = [...STRATEGIES.map((s) => ({ name: s.name, s: result.byStrategy[s.id] })), { name: "Fully consistent reads", s: result.fullyConsistent }];
  const safe = all.filter((x) => x.s.staleAllows === 0);
  const best = [...safe].sort((a, b) => a.s.sqliteReads - b.s.sqliteReads)[0];
  const leaky = all.filter((x) => x.s.staleAllows > 0).sort((a, b) => a.s.maxStaleSec - b.s.maxStaleSec);
  return (
    <div className="principle recommendation" role="note">
      <strong>On this log:</strong>{" "}
      {best ? (
        <>
          {listOf(safe.map((x) => x.name))} never let a revoked user keep access. Of these, <strong>{best.name}</strong> sent the fewest checks to SQLite (
          {pct(best.s.sqliteReads, best.s.checks)}).
        </>
      ) : (
        "every strategy served at least one stale ALLOW."
      )}{" "}
      {leaky.length > 0 && (
        <>
          {leaky.map((x, i) => (
            <span key={x.name}>
              {i > 0 && "; "}
              {x.name} kept a revoked user's access for up to <strong>{fmtSec(x.s.maxStaleSec)}</strong> ({pct(x.s.sqliteReads, x.s.checks)} of checks on
              SQLite)
            </span>
          ))}
          . If your revocation requirement tolerates that window, a cheaper strategy may be enough.
        </>
      )}
    </div>
  );
}
