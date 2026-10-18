import { useEffect, useMemo, useRef, useState } from "react";
import type { LabEvent } from "../types";

interface NodeDef {
  id: string;
  x: number;
  y: number;
  label: string;
  sub: string;
}

const W = 150;
const H = 46;

const NODES: NodeDef[] = [
  { id: "client", x: 75, y: 220, label: "Client / app", sub: "Alice, Bob" },
  { id: "writeapi", x: 270, y: 80, label: "Write API", sub: "grants & revokes" },
  { id: "content", x: 270, y: 220, label: "Content store", sub: "always fresh" },
  { id: "checkapi", x: 270, y: 360, label: "Check API", sub: "engine + defenses" },
  { id: "primary", x: 490, y: 80, label: "Primary store", sub: "source of truth" },
  { id: "audit", x: 490, y: 220, label: "Audit log", sub: "+ oracle verdict" },
  { id: "changelog", x: 710, y: 50, label: "Changelog", sub: "bulk, ordered" },
  { id: "fastlane", x: 710, y: 140, label: "Watermark lane", sub: "tiny, fast" },
  { id: "replica-1", x: 915, y: 240, label: "replica-1", sub: "" },
  { id: "replica-2", x: 915, y: 320, label: "replica-2", sub: "" },
  { id: "replica-3", x: 915, y: 400, label: "replica-3", sub: "" },
];
const REPLICAS = ["replica-1", "replica-2", "replica-3"];
const byId = Object.fromEntries(NODES.map((n) => [n.id, n]));

const EDGES: [string, string][] = [
  ["client", "writeapi"],
  ["client", "content"],
  ["client", "checkapi"],
  ["writeapi", "primary"],
  ["writeapi", "audit"],
  ["checkapi", "audit"],
  ["checkapi", "primary"],
  ["primary", "changelog"],
  ["primary", "fastlane"],
  ...REPLICAS.flatMap((r): [string, string][] => [
    ["changelog", r],
    ["fastlane", r],
    ["checkapi", r],
  ]),
];

const edgeKey = (a: string, b: string) => [a, b].sort().join("|");

interface Props {
  events: LabEvent[];
  title?: string;
}

export function FlowDiagram({ events, title }: Props) {
  const [idx, setIdx] = useState(events.length - 1);
  const [playing, setPlaying] = useState(false);
  const listRef = useRef<HTMLOListElement>(null);

  // Start every new run at the beginning and auto-play it.
  useEffect(() => {
    setIdx(0);
    setPlaying(events.length > 0);
  }, [events]);

  useEffect(() => {
    if (!playing) return;
    if (idx >= events.length - 1) {
      setPlaying(false);
      return;
    }
    const timer = setTimeout(() => setIdx((i) => i + 1), 650);
    return () => clearTimeout(timer);
  }, [playing, idx, events.length]);

  useEffect(() => {
    listRef.current?.querySelector('[aria-current="step"]')?.scrollIntoView({ block: "nearest" });
  }, [idx]);

  const state = useMemo(() => {
    const s = {
      primaryRev: 0,
      applied: {} as Record<string, number>,
      watermark: {} as Record<string, number>,
      cut: {} as Record<string, boolean>,
    };
    for (const e of events.slice(0, idx + 1)) {
      if (e.kind === "write" && e.to === "primary" && e.rev) s.primaryRev = Math.max(s.primaryRev, e.rev);
      if (e.kind === "replicate" && e.rev) s.applied[e.to] = Math.max(s.applied[e.to] ?? 0, e.rev);
      if (e.kind === "watermark" && e.from === "fastlane" && e.rev) s.watermark[e.to] = Math.max(s.watermark[e.to] ?? 0, e.rev);
      if (e.kind === "partition") s.cut[e.to] = true;
    }
    return s;
  }, [events, idx]);

  const current = events[idx];
  const activeKey = current ? edgeKey(current.from, current.to) : "";
  const sev = current?.severity ?? "info";

  if (events.length === 0) {
    return (
      <section className="card diagram">
        <h3>Data flow</h3>
        <p className="muted">Run a step to see how data and checks move between components.</p>
      </section>
    );
  }

  const from = current && byId[current.from];
  const to = current && byId[current.to];

  return (
    <section className="card diagram" aria-label="Data flow diagram">
      <div className="diagram-head">
        <h3>{title ?? "Data flow"}</h3>
        <span className="muted mono">
          t = {current?.t ?? 0} ms · primary rev {state.primaryRev}
        </span>
      </div>
      <div className="diagram-scroll">
        <svg viewBox="0 0 1000 440" role="img" aria-label="Components of the authorization system and the current data flow">
          <defs>
            <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0,0 L10,5 L0,10 z" className={`arrowhead sev-${sev}`} />
            </marker>
          </defs>
          {EDGES.map(([a, b]) => {
            const na = byId[a];
            const nb = byId[b];
            const cut = (REPLICAS.includes(b) && (a === "changelog" || a === "fastlane") && state.cut[b]) || false;
            return (
              <line
                key={a + b}
                x1={na.x}
                y1={na.y}
                x2={nb.x}
                y2={nb.y}
                className={`edge ${edgeKey(a, b) === activeKey ? "edge-active sev-" + sev : ""} ${cut ? "edge-cut" : ""}`}
              />
            );
          })}
          {from && to && (
            <line
              x1={from.x}
              y1={from.y}
              x2={(from.x + to.x) / 2}
              y2={(from.y + to.y) / 2}
              className={`edge-arrow sev-${sev}`}
              markerEnd="url(#arrow)"
            />
          )}
          {from && to && (
            <circle key={current.seq} r="7" className={`packet sev-${sev}`}>
              <animateMotion dur="0.55s" fill="freeze" path={`M${from.x},${from.y} L${to.x},${to.y}`} />
            </circle>
          )}
          {NODES.map((n) => {
            const isReplica = REPLICAS.includes(n.id);
            const active = current && (current.from === n.id || current.to === n.id);
            const behind = isReplica && (state.applied[n.id] ?? 0) < state.primaryRev;
            let sub = n.sub;
            if (isReplica) {
              sub = `rev ${state.applied[n.id] ?? 0}` + (state.watermark[n.id] ? ` · wm ${state.watermark[n.id]}` : "");
            } else if (n.id === "primary") {
              sub = `rev ${state.primaryRev}`;
            }
            return (
              <g key={n.id} className={`node ${active ? "node-active" : ""} ${state.cut[n.id] ? "node-cut" : ""} ${behind ? "node-behind" : ""}`}>
                <rect x={n.x - W / 2} y={n.y - H / 2} width={W} height={H} rx="8" />
                <text x={n.x} y={n.y - 4} textAnchor="middle" className="node-label">
                  {n.label}
                </text>
                <text x={n.x} y={n.y + 13} textAnchor="middle" className="node-sub">
                  {state.cut[n.id] ? "PARTITIONED · " + sub : sub}
                </text>
              </g>
            );
          })}
        </svg>
      </div>

      <div className={`event-now sev-${sev}`} aria-live="polite">
        <strong>
          {idx + 1}/{events.length} · {current?.kind}
        </strong>{" "}
        <span className="mono">
          {current?.from} → {current?.to}
        </span>
        <div>{current?.label}</div>
      </div>

      <div className="scrubber">
        <button onClick={() => setIdx(0)} aria-label="First event">⏮</button>
        <button onClick={() => setIdx((i) => Math.max(0, i - 1))} aria-label="Previous event">◀</button>
        <button className="primary-btn" onClick={() => (idx >= events.length - 1 ? (setIdx(0), setPlaying(true)) : setPlaying((p) => !p))}>
          {playing ? "Pause" : idx >= events.length - 1 ? "Replay" : "Play"}
        </button>
        <button onClick={() => setIdx((i) => Math.min(events.length - 1, i + 1))} aria-label="Next event">▶</button>
        <button onClick={() => setIdx(events.length - 1)} aria-label="Last event">⏭</button>
        <input
          type="range"
          min={0}
          max={events.length - 1}
          value={idx}
          onChange={(e) => {
            setPlaying(false);
            setIdx(Number(e.target.value));
          }}
          aria-label="Timeline position"
        />
      </div>

      <ol className="event-list" ref={listRef}>
        {events.map((e, i) => (
          <li
            key={e.seq}
            aria-current={i === idx ? "step" : undefined}
            className={`sev-${e.severity ?? "info"} ${i > idx ? "future" : ""}`}
            onClick={() => {
              setPlaying(false);
              setIdx(i);
            }}
          >
            <span className="mono t">{e.t} ms</span>
            <span className="mono route">
              {e.from} → {e.to}
            </span>
            <span>{e.label}</span>
          </li>
        ))}
      </ol>
      <p className="legend muted">
        <span className="dot sev-ok" /> fresh / defense worked <span className="dot sev-warn" /> revocation / cost{" "}
        <span className="dot sev-danger" /> stale or blocked · <em>rev</em> = applied revision, <em>wm</em> = revocation watermark received
      </p>
    </section>
  );
}
