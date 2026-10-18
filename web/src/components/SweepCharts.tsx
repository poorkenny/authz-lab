import { useState } from "react";
import type { Mode, SweepPoint, SweepResult } from "../types";

const MODES: Mode[] = ["naive", "tokens", "guarded", "full"];
const MODE_LABEL: Record<Mode, string> = {
  naive: "naive",
  tokens: "tokens only",
  guarded: "guarded",
  full: "fully consistent",
};
// Categorical slots in fixed order: color follows the mode, never its rank.
const MODE_VAR: Record<Mode, string> = {
  naive: "var(--series-1)",
  tokens: "var(--series-2)",
  guarded: "var(--series-3)",
  full: "var(--series-4)",
};

interface Metric {
  key: string;
  title: string;
  unit: string;
  value: (p: SweepPoint) => number;
  fmt: (v: number) => string;
}

const METRICS: Metric[] = [
  { key: "p50", title: "p50 check latency", unit: "ms", value: (p) => p.p50Ms, fmt: (v) => v.toFixed(1) },
  { key: "p99", title: "p99 check latency", unit: "ms", value: (p) => p.p99Ms, fmt: (v) => v.toFixed(1) },
  { key: "primary", title: "Checks served by the primary", unit: "%", value: (p) => p.primaryLoadPct, fmt: (v) => v.toFixed(1) },
  { key: "stale", title: "Stale ALLOWs (security failures)", unit: "count", value: (p) => p.staleAllows, fmt: (v) => String(v) },
];

const CW = 460;
const CH = 230;
const PAD = { l: 48, r: 92, t: 14, b: 34 };

function niceMax(v: number) {
  if (v <= 0) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

function Chart({ metric, lags, series }: { metric: Metric; lags: number[]; series: Record<Mode, SweepPoint[]> }) {
  const [hover, setHover] = useState<number | null>(null);
  const all = MODES.flatMap((m) => series[m].map(metric.value));
  const yMax = niceMax(Math.max(...all));
  const pw = CW - PAD.l - PAD.r;
  const ph = CH - PAD.t - PAD.b;
  // Lags are not evenly spaced (0…5000), so the x axis is ordinal.
  const x = (i: number) => PAD.l + (lags.length === 1 ? pw / 2 : (i * pw) / (lags.length - 1));
  const y = (v: number) => PAD.t + ph - (v / yMax) * ph;
  const ticks = [0, yMax / 2, yMax];

  // Direct labels at line ends, nudged apart so they don't collide.
  const ends = MODES.map((m) => ({ m, y: y(metric.value(series[m][series[m].length - 1])) })).sort((a, b) => a.y - b.y);
  // Push overlapping labels down, then back up so none falls below the axis.
  for (let i = 1; i < ends.length; i++) ends[i].y = Math.max(ends[i].y, ends[i - 1].y + 13);
  ends[ends.length - 1].y = Math.min(ends[ends.length - 1].y, PAD.t + ph - 4);
  for (let i = ends.length - 2; i >= 0; i--) ends[i].y = Math.min(ends[i].y, ends[i + 1].y - 13);

  return (
    <figure className="chart">
      <figcaption>
        {metric.title} <span className="muted">({metric.unit})</span>
      </figcaption>
      <div className="chart-wrap">
        <svg viewBox={`0 0 ${CW} ${CH}`} role="img" aria-label={`${metric.title} by replication lag for each mode`} onMouseLeave={() => setHover(null)}>
          {ticks.map((t) => (
            <g key={t}>
              <line x1={PAD.l} x2={PAD.l + pw} y1={y(t)} y2={y(t)} className={t === 0 ? "axis" : "grid"} />
              <text x={PAD.l - 6} y={y(t) + 4} textAnchor="end" className="tick">
                {metric.fmt(t)}
              </text>
            </g>
          ))}
          {lags.map((l, i) => (
            <text key={l} x={x(i)} y={CH - PAD.b + 16} textAnchor="middle" className="tick">
              {l >= 1000 ? `${l / 1000}s` : `${l}`}
            </text>
          ))}
          <text x={PAD.l + pw / 2} y={CH - 2} textAnchor="middle" className="tick">
            replication lag (ms)
          </text>
          {hover !== null && <line x1={x(hover)} x2={x(hover)} y1={PAD.t} y2={PAD.t + ph} className="crosshair" />}
          {MODES.map((m) => (
            <g key={m}>
              <polyline
                fill="none"
                stroke={MODE_VAR[m]}
                strokeWidth="2"
                strokeLinejoin="round"
                points={series[m].map((p, i) => `${x(i)},${y(metric.value(p))}`).join(" ")}
              />
              {series[m].map((p, i) => (
                <circle key={i} cx={x(i)} cy={y(metric.value(p))} r={hover === i ? 5 : 4} fill={MODE_VAR[m]} className="marker" />
              ))}
            </g>
          ))}
          {ends.map(({ m, y: ly }) => (
            <g key={m}>
              <line x1={x(lags.length - 1) + 6} x2={x(lags.length - 1) + 14} y1={ly} y2={ly} stroke={MODE_VAR[m]} strokeWidth="2" />
              <text x={x(lags.length - 1) + 18} y={ly + 4} className="direct-label">
                {MODE_LABEL[m]}
              </text>
            </g>
          ))}
          {lags.map((_, i) => (
            <rect
              key={i}
              x={x(i) - pw / (2 * Math.max(1, lags.length - 1))}
              y={PAD.t}
              width={pw / Math.max(1, lags.length - 1)}
              height={ph}
              fill="transparent"
              onMouseEnter={() => setHover(i)}
            />
          ))}
        </svg>
        {hover !== null && (
          <div className="tooltip" style={{ left: `${(x(hover) / CW) * 100}%` }}>
            <div className="tooltip-title">lag {lags[hover]} ms</div>
            {MODES.map((m) => (
              <div key={m} className="tooltip-row">
                <span className="swatch" style={{ background: MODE_VAR[m] }} />
                <span>{MODE_LABEL[m]}</span>
                <strong className="mono">
                  {metric.fmt(metric.value(series[m][hover]))} {metric.unit === "count" ? "" : metric.unit}
                </strong>
              </div>
            ))}
          </div>
        )}
      </div>
    </figure>
  );
}

export function SweepCharts({ result }: { result: SweepResult }) {
  const [table, setTable] = useState(false);
  const lags = [...new Set(result.points.map((p) => p.lagMs))];
  const series = Object.fromEntries(
    MODES.map((m) => [m, lags.map((l) => result.points.find((p) => p.mode === m && p.lagMs === l)!)]),
  ) as Record<Mode, SweepPoint[]>;
  const g = series.guarded;
  const ack = g.length ? g[0].revokeAckMs : 0;

  return (
    <section className="card sweep" aria-label="Lag sweep results">
      <div className="diagram-head">
        <h3>Trade-offs across replication lag</h3>
        <button onClick={() => setTable((t) => !t)}>{table ? "Show charts" : "Show table"}</button>
      </div>
      <div className="legend-row" role="list">
        {MODES.map((m) => (
          <span key={m} role="listitem">
            <span className="swatch" style={{ background: MODE_VAR[m] }} /> {MODE_LABEL[m]}
          </span>
        ))}
      </div>
      {table ? (
        <div className="table-scroll">
          <table className="data">
            <thead>
              <tr>
                <th>mode</th>
                <th>lag ms</th>
                <th>checks</th>
                <th>p50 ms</th>
                <th>p99 ms</th>
                <th>served locally %</th>
                <th>primary %</th>
                <th>stale ALLOWs</th>
                <th>revoke ack ms</th>
              </tr>
            </thead>
            <tbody>
              {result.points.map((p) => (
                <tr key={p.mode + p.lagMs}>
                  <td>{MODE_LABEL[p.mode]}</td>
                  <td className="mono">{p.lagMs}</td>
                  <td className="mono">{p.checks}</td>
                  <td className="mono">{p.p50Ms.toFixed(1)}</td>
                  <td className="mono">{p.p99Ms.toFixed(1)}</td>
                  <td className="mono">{p.localPct.toFixed(1)}</td>
                  <td className="mono">{p.primaryLoadPct.toFixed(1)}</td>
                  <td className={`mono ${p.staleAllows > 0 ? "txt-danger" : ""}`}>{p.staleAllows}</td>
                  <td className="mono">{p.revokeAckMs.toFixed(0)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="charts">
          {METRICS.map((m) => (
            <Chart key={m.key} metric={m} lags={lags} series={series} />
          ))}
        </div>
      )}
      <ul className="findings">
        <li>
          <strong>guarded</strong> serves {g[0]?.localPct.toFixed(0)}% → {g[g.length - 1]?.localPct.toFixed(0)}% of checks locally as lag grows, with{" "}
          <strong>{g.reduce((a, p) => a + p.staleAllows, 0)} stale ALLOWs</strong> in total.
        </li>
        <li>
          <strong>naive</strong> is always fastest and leaks {series.naive.reduce((a, p) => a + p.staleAllows, 0)} times. <strong>tokens only</strong> still leaks{" "}
          {series.tokens.reduce((a, p) => a + p.staleAllows, 0)} times: callers without a fresh token aren't protected.
        </li>
        <li>
          <strong>fully consistent</strong> never leaks, but every check pays the primary RTT and the primary takes 100% of the load.
        </li>
        <li>Write-side cost: guarded acknowledges each revocation after the watermark fan-out (~{ack.toFixed(0)} ms).</li>
      </ul>
    </section>
  );
}
