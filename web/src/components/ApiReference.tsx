import { useEffect, useState } from "react";
import { api } from "../api";

const ENDPOINTS: [string, string, string][] = [
  ["Data store", "GET", "/api/v1/tuples?object=&atRev="],
  ["Data store", "POST", "/api/v1/tuples/write   {writes:[{op:\"touch|delete\", tuple:\"doc:42#viewer@user:alice\"}]}"],
  ["Data store", "GET", "/api/v1/changelog"],
  ["Check engine", "POST", "/api/v1/check   {subject, permission:\"view\", object, replica?, token?}"],
  ["Audit", "GET", "/api/v1/audit?kind=&subject=&object=&runId=&stale=true&limit="],
  ["Audit", "GET", "/api/v1/audit/{id}"],
  ["Audit", "GET", "/api/v1/audit/export"],
  ["Lab", "GET", "/api/v1/lab/state"],
  ["Lab", "GET", "/api/v1/lab/timeline"],
  ["Lab", "GET|PUT", "/api/v1/lab/config"],
  ["Lab", "POST", "/api/v1/lab/reset"],
  ["Lab", "POST", "/api/v1/lab/advance   {ms}"],
  ["Lab", "POST", "/api/v1/lab/partition   {replica, heal?}"],
  ["Lab", "POST", "/api/v1/lab/drill   <Config>"],
  ["Lab", "POST", "/api/v1/lab/experiments/lag-sweep   {lagsMs?, opsPerPoint?, revokeRate?, seed?}"],
];

export function ApiReference() {
  const [schema, setSchema] = useState("");
  useEffect(() => {
    api.schema().then((s) => setSchema(s.schema)).catch(() => {});
  }, []);
  const origin = typeof location !== "undefined" ? location.origin : "";
  const sid = api.session();

  return (
    <section className="card">
      <h3>Mock authorization system API</h3>
      <p className="muted">
        All calls are scoped to a lab session via the <code>X-Lab-Session</code> header. Your browser's session is <code>{sid}</code>, so the calls below act on
        the same world as the UI. Time is simulated: use <code>/lab/advance</code> to let replication progress.
      </p>
      <div className="table-scroll">
        <table className="data">
          <thead>
            <tr>
              <th>layer</th>
              <th>method</th>
              <th>path / body</th>
            </tr>
          </thead>
          <tbody>
            {ENDPOINTS.map(([layer, m, p]) => (
              <tr key={m + p}>
                <td>{layer}</td>
                <td className="mono">{m}</td>
                <td className="mono">{p}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h4>Try it</h4>
      <pre className="code">{`H='X-Lab-Session: ${sid}'
curl -s -H "$H" -X POST ${origin}/api/v1/lab/reset
curl -s -H "$H" -X PUT ${origin}/api/v1/lab/config -d '{"sources":{"replicationLag":{"enabled":true,"lagMs":3000}}}'
curl -s -H "$H" -X POST ${origin}/api/v1/tuples/write -d '{"writes":[{"op":"delete","tuple":"doc:42#viewer@user:alice"}]}'
curl -s -H "$H" -X POST ${origin}/api/v1/check -d '{"subject":"user:alice","permission":"view","object":"doc:42","replica":"replica-2"}'
curl -s -H "$H" "${origin}/api/v1/audit?stale=true"`}</pre>
      <h4>Schema</h4>
      <pre className="code">{schema}</pre>
    </section>
  );
}
