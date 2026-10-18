import { useState } from "react";
import { api } from "../api";
import type { StrategyStats, SuiteResult } from "../types";
import { STRATEGIES, byId } from "../strategies";

export function TestsPanel() {
  const [result, setResult] = useState<SuiteResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function run() {
    setBusy(true);
    setError("");
    try {
      setResult(await api.runTests());
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const rows: [string, (s: StrategyStats) => string, "errors"?][] = [
    ["checks", (s) => s.checks.toLocaleString()],
    ["wrong decisions", (s) => s.wrong.toLocaleString(), "errors"],
    ["stale ALLOWs", (s) => s.staleAllows.toLocaleString(), "errors"],
    ["wrong while an invalidation was in flight", (s) => s.inWindow.toLocaleString()],
    ["cache hit rate", (s) => `${s.hitRatePct.toFixed(1)}%`],
    ["fallbacks to SQLite", (s) => s.fallbacks.toLocaleString()],
    ["mean check latency", (s) => `${s.meanMs.toFixed(2)} ms`],
  ];

  return (
    <section className="card" aria-labelledby="tests-h">
      <h2 id="tests-h">4 · Correctness tests</h2>
      <p>
        The server runs the suite on demand. It's the same code that <code>go test ./internal/demo</code> runs in CI: scenario tests for the story above, and a
        randomized property test that replays identical workloads against all three strategies on SQLite.
      </p>
      <button className="primary-btn" onClick={run} disabled={busy}>
        {busy ? "Running…" : result ? "Run again" : "Run correctness tests"}
      </button>
      {error && <p className="txt-bad">{error}</p>}

      {result && (
        <>
          <p className={`suite-summary ${result.passed ? "ok" : "bad"}`}>
            <span aria-hidden="true">{result.passed ? "✓" : "✕"}</span> {result.cases.filter((c) => c.passed).length}/{result.cases.length} passed in{" "}
            {result.durationMs.toFixed(0)} ms
          </p>
          <ol className="tests">
            {result.cases.map((c) => (
              <li key={c.name} className={c.passed ? "ok" : "bad"}>
                <span className="test-icon" aria-label={c.passed ? "passed" : "failed"}>
                  {c.passed ? "✓" : "✕"}
                </span>
                <div>
                  <strong>{c.name}</strong> <span className="tag">{c.kind}</span>
                  {c.strategy && <span className={`tag strat-${byId[c.strategy].verdict}`}>{byId[c.strategy].short}</span>}
                  <div className="muted">{c.description}</div>
                  {c.details.map((d) => (
                    <div key={d} className="mono small detail">
                      {d}
                    </div>
                  ))}
                </div>
              </li>
            ))}
          </ol>

          <h3>Property test: identical workloads, three strategies</h3>
          <p className="muted small">
            {result.property.seeds} seeds × {result.property.opsPerRun} operations, requests 0-40 ms apart, {result.property.writes.toLocaleString()}{" "}
            grants/revokes in total.
          </p>
          <div className="table-scroll">
            <table className="data">
              <thead>
                <tr>
                  <th scope="col"></th>
                  {STRATEGIES.map((s) => (
                    <th key={s.id} scope="col">
                      {s.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {rows.map(([label, f, kind]) => (
                  <tr key={label}>
                    <th scope="row">{label}</th>
                    {STRATEGIES.map((s) => {
                      const v = f(result.property.byStrategy[s.id]);
                      return (
                        <td key={s.id} className={`mono ${kind === "errors" ? (v === "0" ? "txt-ok" : "txt-bad") : ""}`}>
                          {v}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            Invalidation and revision-aware cost about the same here (both re-read SQLite after a change). The difference is the guarantee: every invalidation
            mistake happened inside a propagation window, and revision-aware made none.
          </p>
        </>
      )}
    </section>
  );
}
