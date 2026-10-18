import type { CheckResult, CompareRow } from "../types";
import { byId } from "../strategies";
import { fmtMs } from "./FlowDiagram";

// x positions on a broken time axis: the race window is zoomed in, the
// "1 s later" check sits after the break.
const X = { commit: 190, soon: 330, arrive: 560, brk: 640, later: 735 };
const ROW_Y0 = 120;
const ROW_H = 46;

function Mark({ r, x, y }: { r: CheckResult; x: number; y: number }) {
  return (
    <g>
      <circle cx={x} cy={y} r="9" className={r.correct ? "mark-ok" : "mark-bad"} />
      <text x={x} y={y + 4} textAnchor="middle" className="mark-icon">
        {r.correct ? "✓" : "✕"}
      </text>
      <text x={x + 16} y={y + 4} className="mark-label">
        {r.decision} · {fmtMs(r.latencyMs)} ms
      </text>
    </g>
  );
}

export function RaceTimeline({ rows }: { rows: CompareRow[] }) {
  if (rows.length === 0) return <p className="muted">Loading timeline…</p>;
  const h = ROW_Y0 + rows.length * ROW_H;
  const inv = rows.find((r) => r.strategy === "invalidation")?.soon.invalidation;
  const commitT = inv?.sentAt ?? 1000;
  const arriveT = inv?.deliverAt ?? 1080;

  return (
    <figure className="timeline">
      <div className="diagram-scroll">
        <svg viewBox={`0 0 900 ${h}`} role="img" aria-label="Timeline: SQLite commit, invalidation arrival, and the result of Alice's checks 10 ms and 1 s after the revoke, for each strategy">
          <rect x={X.commit} y={34} width={X.arrive - X.commit} height={h - 34} className="window" />
          <text x={(X.commit + X.arrive) / 2} y={52} textAnchor="middle" className="window-label">
            race window: invalidation in flight ({arriveT - commitT} ms)
          </text>

          <line x1={150} x2={X.brk - 12} y1={84} y2={84} className="axis" />
          <line x1={X.brk + 12} x2={880} y1={84} y2={84} className="axis" />
          <text x={X.brk} y={88} textAnchor="middle" className="tick">
            ⋯
          </text>
          {[
            { x: X.commit, t: commitT, label: "SQLite commit (revoke)" },
            { x: X.soon, t: commitT + 10, label: "check" },
            { x: X.arrive, t: arriveT, label: "invalidation arrives" },
            { x: X.later, t: commitT + 1010, label: "check" },
          ].map((m) => (
            <g key={m.x}>
              <line x1={m.x} x2={m.x} y1={78} y2={h - 8} className="tick-line" />
              <text x={m.x} y={74} textAnchor="middle" className="tick">
                t={m.t} ms
              </text>
              <text x={m.x} y={102} textAnchor="middle" className="tick strong">
                {m.label}
              </text>
            </g>
          ))}

          {rows.map((r, i) => {
            const y = ROW_Y0 + i * ROW_H + 16;
            return (
              <g key={r.strategy}>
                <text x={12} y={y + 4} className="row-label">
                  {byId[r.strategy].short}
                </text>
                <Mark r={r.soon} x={X.soon} y={y} />
                <Mark r={r.later} x={X.later} y={y} />
              </g>
            );
          })}
        </svg>
      </div>
      <figcaption className="muted small">
        Alice was revoked at t={commitT} ms. Each row shows what she gets 10 ms and 1 s later under each strategy (live results from the server).
      </figcaption>
    </figure>
  );
}
