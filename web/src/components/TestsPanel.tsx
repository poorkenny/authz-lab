import { useState } from "react";
import { ApiError, api } from "../api";
import type { StrategyStats, SuiteResult } from "../types";
import { STRATEGIES, byId } from "../strategies";
import { LogReplay } from "./LogReplay";

// TestsPanel is the benchmark section: the default scenarios (the correctness
// suite) and, with FEATURE_LOG_REPLAY, a replay of an uploaded log.
export function TestsPanel({ section, logReplay }: { section: number; logReplay: boolean }) {
  const [mode, setMode] = useState<"default" | "upload">("default");
  const [result, setResult] = useState<SuiteResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  async function run() {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      setResult(await api.runTests());
    } catch (e) {
      // 409: another reviewer's run is in progress (one run at a time per server).
      if (e instanceof ApiError && e.status === 409) setNotice(e.message);
      else setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  const rows: [string, (s: StrategyStats) => string, "errors"?][] = [
    ["checks", (s) => s.checks.toLocaleString()],
    ["wrong decisions", (s) => s.wrong.toLocaleString(), "errors"],
    ["stale ALLOWs", (s) => s.staleAllows.toLocaleString(), "errors"],
    ["wrong while the invalidation was queued", (s) => s.inWindow.toLocaleString()],
    ["cache hit rate", (s) => `${s.hitRatePct.toFixed(1)}%`],
    ["fallbacks to SQLite", (s) => s.fallbacks.toLocaleString()],
    ["p50 check latency", (s) => `${s.p50Ms.toFixed(2)} ms`],
    ["p99 check latency", (s) => `${s.p99Ms.toFixed(2)} ms`],
  ];

  return (
    <section className="card" aria-labelledby="tests-h">
      <h2 id="tests-h">{section} · Benchmark tests</h2>
      {logReplay && (
        <div className="mode-row">
          <div className="segmented" role="radiogroup" aria-label="Benchmark input">
            <button role="radio" aria-checked={mode === "default"} className={mode === "default" ? "on" : ""} onClick={() => setMode("default")}>
              Default test scenarios
            </button>
            <button role="radio" aria-checked={mode === "upload"} className={mode === "upload" ? "on" : ""} onClick={() => setMode("upload")}>
              Upload your logs
            </button>
          </div>
        </div>
      )}
      {logReplay && mode === "upload" ? (
        <LogReplay />
      ) : (
        <>
          <p>
            The server runs the default scenarios on demand. It's the same code that <code>go test ./internal/demo</code> runs in CI: scenario tests for the
            stories above, and a randomized property test that replays identical workloads against all three strategies on SQLite.
          </p>
          <button className="primary-btn" onClick={run} disabled={busy}>
            {busy ? "Running…" : result ? "Run again" : "Run default scenarios"}
          </button>
          {notice && (
            <p className="sim-note" role="status">
              {notice}
            </p>
          )}
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
                {result.property.seeds} seeds × {result.property.opsPerRun} operations: {result.property.writes.toLocaleString()} grants/revokes and{" "}
                {result.property.deliveries.toLocaleString()} invalidation deliveries, interleaved with checks in a seeded random order.
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
                Invalidation looks cheaper than revision-aware because it keeps serving cached entries until their event is delivered. Those are exactly the
                reads where it was wrong (every one happened while the user's invalidation was still queued). Revision-aware pays an extra SQLite round trip
                right after each change and is never wrong.
              </p>
            </>
          )}
        </>
      )}
    </section>
  );
}
