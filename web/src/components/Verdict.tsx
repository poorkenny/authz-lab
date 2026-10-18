import type { CheckResult, DrillResult, Outcome } from "../types";

const OUTCOME_LABEL: Record<Outcome, string> = {
  leaked: "Stale ALLOW: revocation bypassed",
  safe: "Safe: revocation enforced",
  safe_false_deny: "Safe, with a false deny",
};
const OUTCOME_ICON: Record<Outcome, string> = { leaked: "✕", safe: "✓", safe_false_deny: "!" };

export function OutcomeBadge({ outcome }: { outcome: Outcome }) {
  return (
    <span className={`badge outcome-${outcome}`}>
      <span aria-hidden="true">{OUTCOME_ICON[outcome]}</span> {OUTCOME_LABEL[outcome]}
    </span>
  );
}

function EvidenceTable({ c }: { c: CheckResult }) {
  const e = c.evidence;
  const rows: [string, React.ReactNode][] = [
    ["decision", <strong className={c.stale ? "txt-danger" : c.falseDeny ? "txt-warn" : ""}>{c.decision}</strong>],
    ["primary would say", c.oracleDecision],
    ["served by", e.servedBy],
    ["path", e.path],
    ["snapshot rev", e.snapshotRev],
    ["required rev", e.requiredRev || "none"],
    ["token rev", e.tokenRev || "none"],
    ["watermark rev", e.watermarkRev || "none"],
    ["primary rev at start", e.primaryRevAtStart],
    ["latency", `${e.latencyMs.toFixed(1)} ms`],
    ["defense applied", e.defenseApplied || "none"],
  ];
  return (
    <table className="evidence">
      <tbody>
        {rows.map(([k, v]) => (
          <tr key={k}>
            <th scope="row">{k}</th>
            <td className="mono">{v}</td>
          </tr>
        ))}
        {e.resolution.length > 0 && (
          <tr>
            <th scope="row">resolution path</th>
            <td className="mono">{e.resolution.join(" → ")}</td>
          </tr>
        )}
      </tbody>
    </table>
  );
}

export function VerdictPanel({ result }: { result: DrillResult }) {
  const v = result.verdict;
  return (
    <section className={`card verdict outcome-border-${v.outcome}`} aria-live="polite">
      <OutcomeBadge outcome={v.outcome} />
      <p className="headline">{v.headline}</p>

      {v.causes.length > 0 && (
        <>
          <h4>Why it happened</h4>
          <ul>{v.causes.map((c) => <li key={c}>{c}</li>)}</ul>
        </>
      )}
      {v.defenses.length > 0 && (
        <>
          <h4>What prevented it</h4>
          <ul>{v.defenses.map((c) => <li key={c}>{c}</li>)}</ul>
        </>
      )}
      {v.tradeoffs.length > 0 && (
        <>
          <h4>Cost</h4>
          <ul>{v.tradeoffs.map((c) => <li key={c}>{c}</li>)}</ul>
        </>
      )}

      <div className="evidence-grid">
        <div>
          <h4>Final check: Alice (revoked)</h4>
          <EvidenceTable c={result.final} />
        </div>
        <div>
          <h4>Control check: Bob (still owner)</h4>
          <EvidenceTable c={result.control} />
        </div>
      </div>

      <details>
        <summary>Drill steps ({result.steps.length})</summary>
        <ol className="steps">
          {result.steps.map((s, i) => (
            <li key={i}>
              <span className="mono t">{s.t} ms</span> {s.title}
              {s.check && (
                <span className={`mono ${s.check.stale ? "txt-danger" : ""}`}>
                  {" "}
                  → {s.check.decision} ({s.check.evidence.path}, rev {s.check.evidence.snapshotRev})
                </span>
              )}
              {s.detail && <div className="muted small">{s.detail}</div>}
            </li>
          ))}
        </ol>
      </details>
    </section>
  );
}
