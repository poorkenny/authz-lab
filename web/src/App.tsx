import { useEffect, useState } from "react";
import { api } from "./api";
import { CHAPTERS, STEPS, defaultConfig, type ProcedureStep } from "./procedure";
import type { Config, DrillResult, SweepResult } from "./types";
import { Composer } from "./components/Composer";
import { FlowDiagram } from "./components/FlowDiagram";
import { OutcomeBadge, VerdictPanel } from "./components/Verdict";
import { SweepCharts } from "./components/SweepCharts";
import { AuditLog } from "./components/AuditLog";
import { ApiReference } from "./components/ApiReference";

type Tab = "guided" | "free" | "audit" | "api";
type Progress = Record<string, "match" | "mismatch">;

function load<T>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem(key);
    return v ? (JSON.parse(v) as T) : fallback;
  } catch {
    return fallback;
  }
}
function save(key: string, v: unknown) {
  try {
    localStorage.setItem(key, JSON.stringify(v));
  } catch {
    /* storage unavailable: progress just isn't remembered */
  }
}

export default function App() {
  const [tab, setTab] = useState<Tab>(() => (location.hash.slice(1) as Tab) || "guided");
  useEffect(() => {
    history.replaceState(null, "", "#" + tab);
  }, [tab]);

  return (
    <div className="app">
      <header className="top">
        <div>
          <h1>AuthZ Revocation Lab</h1>
          <p className="muted">Can you deliver low-latency authorization without letting stale replicas bypass security-critical revocations?</p>
        </div>
        <nav className="tabs" aria-label="Sections">
          {(
            [
              ["guided", "Guided review"],
              ["free", "Free play"],
              ["audit", "Audit log"],
              ["api", "API"],
            ] as [Tab, string][]
          ).map(([id, label]) => (
            <button key={id} className={tab === id ? "tab active" : "tab"} aria-current={tab === id ? "page" : undefined} onClick={() => setTab(id)}>
              {label}
            </button>
          ))}
        </nav>
      </header>
      <main>
        {tab === "guided" && <Guided />}
        {tab === "free" && <FreePlay />}
        {tab === "audit" && <AuditLog />}
        {tab === "api" && <ApiReference />}
      </main>
      <footer className="muted small">
        Mock system with simulated time; every run is deterministic. Each browser gets an isolated lab session.
      </footer>
    </div>
  );
}

function Guided() {
  const [idx, setIdx] = useState(() => Math.min(load("authz-lab-step", 0), STEPS.length - 1));
  const [progress, setProgress] = useState<Progress>(() => load("authz-lab-progress", {}));
  const [configs, setConfigs] = useState<Record<string, Config>>({});
  const [drills, setDrills] = useState<Record<string, DrillResult>>({});
  const [sweep, setSweep] = useState<SweepResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [customize, setCustomize] = useState(false);

  const step = STEPS[idx];
  const chapter = CHAPTERS.find((c) => c.id === step.chapter)!;
  const config = configs[step.id] ?? step.config ?? defaultConfig();
  const modified = !!configs[step.id];
  const result = drills[step.id];

  useEffect(() => save("authz-lab-step", idx), [idx]);
  useEffect(() => save("authz-lab-progress", progress), [progress]);
  useEffect(() => {
    setCustomize(false);
    setError("");
  }, [idx]);

  const mark = (s: ProcedureStep, ok: boolean) => setProgress((p) => ({ ...p, [s.id]: ok ? "match" : "mismatch" }));

  async function run() {
    setBusy(true);
    setError("");
    try {
      if (step.kind === "sweep") {
        const r = await api.lagSweep({});
        setSweep(r);
        const stale = (m: string) => r.points.filter((p) => p.mode === m).reduce((a, p) => a + p.staleAllows, 0);
        mark(step, stale("guarded") === 0 && stale("naive") > 0);
      } else {
        const r = await api.drill(config);
        setDrills((d) => ({ ...d, [step.id]: r }));
        if (!modified) mark(step, r.verdict.outcome === step.expected);
      }
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="guided">
      <aside className="procedure" aria-label="Review procedure">
        {CHAPTERS.map((c) => (
          <div key={c.id} className="chapter">
            <h2>
              {c.id.toUpperCase()} · {c.question}
            </h2>
            <ol>
              {STEPS.map((s, i) =>
                s.chapter !== c.id ? null : (
                  <li key={s.id}>
                    <button className={i === idx ? "step-btn current" : "step-btn"} aria-current={i === idx ? "step" : undefined} onClick={() => setIdx(i)}>
                      <span className={`status status-${progress[s.id] ?? "todo"}`} aria-label={progress[s.id] ?? "not run"}>
                        {progress[s.id] === "match" ? "✓" : progress[s.id] === "mismatch" ? "!" : i + 1}
                      </span>
                      {s.title}
                    </button>
                  </li>
                ),
              )}
            </ol>
          </div>
        ))}
        <button
          className="link small"
          onClick={() => {
            setProgress({});
            setIdx(0);
          }}
        >
          Reset progress
        </button>
      </aside>

      <div className="stage">
        <section className="chapter-banner">
          <span className="eyebrow">{chapter.id.toUpperCase()}</span>
          <h2>{chapter.question}</h2>
          <p>{chapter.summary}</p>
        </section>

        <section className="card step-card">
          <div className="step-head">
            <h3>
              Step {idx + 1} of {STEPS.length}: {step.title}
            </h3>
            {step.expected && (
              <span className="muted small">
                expected: <OutcomeBadge outcome={step.expected} />
              </span>
            )}
          </div>
          <p>
            <strong>Goal.</strong> {step.goal}
          </p>
          <p>
            <strong>What to look for.</strong> {step.lookFor}
          </p>
          <div className="row">
            <button className="primary-btn" onClick={run} disabled={busy}>
              {busy ? "Running…" : result || (step.kind === "sweep" && sweep) ? "Run again" : "Run step"}
            </button>
            {step.kind === "drill" && (
              <button onClick={() => setCustomize((c) => !c)} aria-expanded={customize}>
                {customize ? "Hide scenario" : "Customize scenario"}
              </button>
            )}
            {modified && (
              <button className="link" onClick={() => setConfigs(({ [step.id]: _, ...rest }) => rest)}>
                Restore step defaults
              </button>
            )}
            <span className="spacer" />
            <button onClick={() => setIdx((i) => Math.max(0, i - 1))} disabled={idx === 0}>
              ← Previous
            </button>
            <button onClick={() => setIdx((i) => Math.min(STEPS.length - 1, i + 1))} disabled={idx === STEPS.length - 1}>
              Next →
            </button>
          </div>
          {error && <p className="txt-danger">{error}</p>}
          {modified && <p className="muted small">You modified this scenario, so the checkpoint isn't scored. Compare what you see with the step's expectation.</p>}

          {(result || (step.kind === "sweep" && sweep)) && (
            <div className={`checkpoint checkpoint-${progress[step.id] ?? "todo"}`}>
              {step.kind === "drill" && result && (
                <>
                  <strong>Checkpoint.</strong> Observed <OutcomeBadge outcome={result.verdict.outcome} />{" "}
                  {!modified && (result.verdict.outcome === step.expected ? "matches the expectation." : "differs from the expectation.")}
                </>
              )}
              {step.kind === "sweep" && (
                <>
                  <strong>Checkpoint.</strong> {progress[step.id] === "match" ? "guarded never leaked while naive did, as expected." : "Unexpected result: see the table."}
                </>
              )}
              {step.question && (
                <p>
                  <strong>Think about it:</strong> {step.question}
                </p>
              )}
              <details>
                <summary>Takeaway</summary>
                <p>{step.takeaway}</p>
              </details>
            </div>
          )}
        </section>

        {customize && step.kind === "drill" && <Composer config={config} onChange={(c) => setConfigs((m) => ({ ...m, [step.id]: c }))} />}

        {step.kind === "drill" && result && (
          <>
            <FlowDiagram events={result.timeline} title={`Data flow: run ${result.runId}`} />
            <VerdictPanel result={result} />
          </>
        )}
        {step.kind === "drill" && !result && <FlowDiagram events={[]} />}
        {step.kind === "sweep" && sweep && <SweepCharts result={sweep} />}
      </div>
    </div>
  );
}

function FreePlay() {
  const [config, setConfig] = useState<Config>(() => load("authz-lab-free-config", defaultConfig()));
  const [result, setResult] = useState<DrillResult | null>(null);
  const [sweep, setSweep] = useState<SweepResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => save("authz-lab-free-config", config), [config]);

  const go = async (f: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await f();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stage free">
      <section className="chapter-banner">
        <span className="eyebrow">Free play</span>
        <h2>Compose any combination of staleness sources and defenses</h2>
        <p>
          The drill grants Alice access to doc:42, lets replication settle, revokes her, and checks again through replica-2. Bob, still the owner, runs a control
          check to expose availability costs.
        </p>
      </section>
      <Composer config={config} onChange={setConfig} />
      <div className="row">
        <button className="primary-btn" disabled={busy} onClick={() => go(async () => setResult(await api.drill(config)))}>
          Run revocation drill
        </button>
        <button disabled={busy} onClick={() => go(async () => setSweep(await api.lagSweep({})))}>
          Run lag sweep (all modes)
        </button>
        <button className="link" onClick={() => setConfig(defaultConfig())}>
          Reset composer
        </button>
        {busy && <span className="muted">Running…</span>}
      </div>
      {error && <p className="txt-danger">{error}</p>}
      {result && (
        <>
          <FlowDiagram events={result.timeline} title={`Data flow: run ${result.runId}`} />
          <VerdictPanel result={result} />
        </>
      )}
      {sweep && <SweepCharts result={sweep} />}
    </div>
  );
}
