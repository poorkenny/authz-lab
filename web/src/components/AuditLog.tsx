import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import type { AuditRecord } from "../types";

export function AuditLog() {
  const [records, setRecords] = useState<AuditRecord[]>([]);
  const [staleOnly, setStaleOnly] = useState(false);
  const [kind, setKind] = useState("");
  const [error, setError] = useState("");

  const load = useCallback(() => {
    const params: Record<string, string> = { limit: "300" };
    if (staleOnly) params.stale = "true";
    if (kind) params.kind = kind;
    api
      .audit(params)
      .then((r) => {
        setRecords(r.records);
        setError("");
      })
      .catch((e: Error) => setError(e.message));
  }, [staleOnly, kind]);

  useEffect(load, [load]);

  return (
    <section className="card">
      <div className="diagram-head">
        <h3>Audit log</h3>
        <div className="row">
          <label>
            <input type="checkbox" checked={staleOnly} onChange={(e) => setStaleOnly(e.target.checked)} /> stale ALLOWs only
          </label>
          <select value={kind} onChange={(e) => setKind(e.target.value)} aria-label="Record kind">
            <option value="">all kinds</option>
            <option value="check">checks</option>
            <option value="write">writes</option>
          </select>
          <button onClick={load}>Refresh</button>
          <a className="button" href="/api/v1/audit/export" onClick={(e) => exportAudit(e)}>
            Export JSON
          </a>
        </div>
      </div>
      <p className="muted">
        Every write and check in your session, newest first. Each check carries an <em>oracle verdict</em>: the same check evaluated at the primary's latest revision.
        A stale ALLOW is a check that returned ALLOW while the oracle says DENY.
      </p>
      {error && <p className="txt-danger">{error}</p>}
      <div className="table-scroll">
        <table className="data">
          <thead>
            <tr>
              <th>run</th>
              <th>t ms</th>
              <th>kind</th>
              <th>what</th>
              <th>decision</th>
              <th>oracle</th>
              <th>served by</th>
              <th>path</th>
              <th>snap / req / wm rev</th>
              <th>latency</th>
              <th>defense</th>
            </tr>
          </thead>
          <tbody>
            {records.map((r) => (
              <tr key={r.id} className={r.stale ? "row-danger" : r.falseDeny ? "row-warn" : ""}>
                <td className="mono">{r.runId}</td>
                <td className="mono">{r.t}</td>
                <td>{r.kind}</td>
                <td className="mono">{r.kind === "write" ? `${r.op} ${r.tuple} → rev ${r.rev}` : `${r.subject} ${r.permission} ${r.object}`}</td>
                <td>
                  {r.decision}
                  {r.stale && <strong className="txt-danger"> ✕ stale</strong>}
                  {r.falseDeny && <strong className="txt-warn"> ! false deny</strong>}
                </td>
                <td>{r.oracleDecision}</td>
                <td>{r.servedBy}</td>
                <td>{r.path}</td>
                <td className="mono">{r.kind === "check" ? `${r.snapshotRev ?? 0} / ${r.requiredRev ?? 0} / ${r.watermarkRev ?? 0}` : ""}</td>
                <td className="mono">{r.latencyMs ? `${r.latencyMs.toFixed(1)} ms` : ""}</td>
                <td>{r.defenseApplied}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

// The export endpoint needs the session header, so fetch it and save the JSON.
async function exportAudit(e: React.MouseEvent) {
  e.preventDefault();
  const res = await fetch("/api/v1/audit/export", { headers: { "X-Lab-Session": api.session() } });
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = "authz-lab-audit.json";
  a.click();
  URL.revokeObjectURL(url);
}
