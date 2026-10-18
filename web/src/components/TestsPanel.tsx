import { useState } from "react";
import { api } from "../api";
import type { ModeStats, SuiteResult } from "../types";

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

  const rows: [string, (s: ModeStats) => string, boolean?][] = [
    ["checks", (s) => s.checks.toLocaleString()],
    ["wrong decisions", (s) => s.wrong.toLocaleString(), true],
    ["stale ALLOWs", (s) => s.staleAllows.toLocaleString(), true],
    ["cache hit rate", (s) => `${s.hitRatePct.toFixed(1)}%`],
    ["fallbacks to primary", (s) => s.fallbacks.toLocaleString()],
    ["mean check latency", (s) => `${s.meanMs.toFixed(2)} ms`],
  ];

  return (
    <section className="card" aria-labelledby="tests-h">
      <h2 id="tests-h">4 · Correctness tests</h2>
      <p>
        The server runs the suite on demand. It's the same code that <code>go test ./internal/demo</code> runs in CI: deterministic scenario tests for the story
        above, plus a randomized property test comparing both modes on identical workloads.
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

          <h3>Property test: same workloads, both modes</h3>
          <p className="muted small">
            {result.property.seeds} seeds × {result.property.opsPerRun} operations, {result.property.writes.toLocaleString()} grants/revokes in total.
          </p>
          <div className="table-scroll">
            <table className="data">
              <thead>
                <tr>
                  <th scope="col"></th>
                  <th scope="col">Plain cache</th>
                  <th scope="col">Revision-aware fallback</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(([label, f, bad]) => (
                  <tr key={label}>
                    <th scope="row">{label}</th>
                    <td className={`mono ${bad && f(result.property.plain) !== "0" ? "txt-bad" : ""}`}>{f(result.property.plain)}</td>
                    <td className={`mono ${bad && f(result.property.revisionAware) === "0" ? "txt-ok" : ""}`}>{f(result.property.revisionAware)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="muted small">
            The trade-off in one line: revision-aware gives up some cache hits (each fallback costs a primary round trip) to get zero wrong decisions.
          </p>
        </>
      )}
    </section>
  );
}
