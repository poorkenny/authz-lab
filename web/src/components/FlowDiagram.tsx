import { useEffect, useState } from "react";
import type { CacheEntry, Hop, State } from "../types";
import { byId } from "../strategies";

interface Box {
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
}

const BOXES: Record<string, Box> = {
  client: { x: 20, y: 70, w: 130, h: 56, label: "Client" },
  checkapi: { x: 300, y: 70, w: 170, h: 56, label: "Check API" },
  cache: { x: 652, y: 40, w: 240, h: 118, label: "Permission cache" },
  queue: { x: 652, y: 236, w: 240, h: 140, label: "Invalidation queue" },
  primary: { x: 300, y: 270, w: 220, h: 100, label: "Primary store (SQLite)" },
  admin: { x: 20, y: 292, w: 130, h: 56, label: "Admin" },
};

const center = (b: Box) => ({ x: b.x + b.w / 2, y: b.y + b.h / 2 });
const edgeKey = (a: string, b: string) => [a, b].sort().join("|");

// Where each edge's latency labels sit.
const LABEL_POS: Record<string, { x: number; y: number; anchor: "start" | "middle" | "end" }> = {
  [edgeKey("client", "checkapi")]: { x: 225, y: 62, anchor: "middle" },
  [edgeKey("checkapi", "cache")]: { x: 474, y: 124, anchor: "start" },
  [edgeKey("checkapi", "primary")]: { x: 384, y: 200, anchor: "end" },
  [edgeKey("admin", "primary")]: { x: 225, y: 345, anchor: "middle" },
  [edgeKey("primary", "queue")]: { x: 586, y: 296, anchor: "middle" },
  [edgeKey("queue", "cache")]: { x: 760, y: 202, anchor: "end" },
};

const EDGES: [string, string][] = [
  ["client", "checkapi"],
  ["checkapi", "cache"],
  ["checkapi", "primary"],
  ["admin", "primary"],
  ["primary", "queue"],
  ["queue", "cache"],
];

export function shortLabel(h: Hop, i: number, hops: Hop[]): string {
  const k = `${h.from}>${h.to}`;
  if (k === "client>checkapi") return "request";
  if (k === "checkapi>client") return "response";
  if (k === "cache>checkapi") return "revision check";
  if (h.kind === "fail" && h.to === "primary") return k === "admin>primary" ? "commit failed" : "SQLite unavailable";
  if (k === "checkapi>primary") return h.kind === "fallback" ? "fallback to SQLite" : h.kind === "strong" ? "fully consistent read" : "SQLite query";
  if (k === "checkapi>cache") return hops.slice(0, i).some((p) => p.from === "checkapi" && p.to === "cache") ? "cache refresh" : "cache lookup";
  if (k === "admin>primary") return "SQLite commit";
  if (k === "primary>queue") return h.kind === "async" ? "enqueue invalidation" : "no invalidation";
  if (k === "queue>cache") return "deliver invalidation";
  return h.label;
}

export const fmtMs = (ms: number) => (ms < 0.1 && ms > 0 ? ms.toFixed(2) : ms.toFixed(1));

export const hopMs = (h: Hop) => (h.kind === "none" ? "—" : h.from === "queue" ? "async" : `${fmtMs(h.ms)} ms`);

const who = (s: string) => s.replace("user:", "");

interface Props {
  hops: Hop[];
  state: State | null;
  actor: string;
  onDeliver?: () => void;
  busy?: boolean;
}

export function FlowDiagram({ hops: hopsProp, state, actor, onDeliver, busy }: Props) {
  const hops = hopsProp ?? [];
  // Reveal hops one at a time so the path is easy to follow.
  const [shown, setShown] = useState(hops.length);
  useEffect(() => {
    setShown(0);
  }, [hops]);
  useEffect(() => {
    if (shown >= hops.length) return;
    const t = setTimeout(() => setShown((n) => n + 1), 450);
    return () => clearTimeout(t);
  }, [shown, hops.length]);

  const visible = hops.slice(0, shown);
  const byEdge = new Map<string, { n: number; text: string; kind?: string }[]>();
  visible.forEach((h, i) => {
    const key = edgeKey(h.from, h.to);
    const list = byEdge.get(key) ?? [];
    list.push({ n: i + 1, text: `${shortLabel(h, i, hops).replace("revision check", "rev check").replace(" invalidation", "")} · ${hopMs(h)}`, kind: h.kind });
    byEdge.set(key, list);
  });
  const current = visible[visible.length - 1];
  const total = visible.reduce((a, h) => a + h.ms, 0);
  const usesQueue = state?.strategy === "invalidation";
  const queued = state?.queued ?? [];
  const isStale = (e: CacheEntry) => e.rev < (state?.docRev[e.object] ?? 0);

  return (
    <figure className="diagram" aria-label="Data flow with per-hop latency">
      <div className="diagram-scroll">
        <svg viewBox="0 0 900 390" role="img" aria-label="Client, Check API, permission cache, invalidation queue, SQLite primary store and admin, with the hops of the last request">
          {EDGES.map(([a, b]) => {
            const ca = center(BOXES[a]);
            const cb = center(BOXES[b]);
            const key = edgeKey(a, b);
            const used = byEdge.get(key);
            const kinds = used?.map((u) => u.kind) ?? [];
            const queueEdge = key === edgeKey("primary", "queue") || key === edgeKey("queue", "cache");
            let cls = "edge";
            if (queueEdge && !usesQueue) cls = "edge none-edge";
            else if (queueEdge && (kinds.includes("async") || (key === edgeKey("queue", "cache") && queued.length > 0))) cls = "edge async";
            else if (used) cls = `edge used ${kinds.includes("stale") || kinds.includes("fail") ? "k-stale" : kinds.includes("fallback") ? "k-fallback" : ""}`;
            const active = current && edgeKey(current.from, current.to) === key;
            return <line key={key} x1={ca.x} y1={ca.y} x2={cb.x} y2={cb.y} className={`${cls} ${active ? "active" : ""}`} />;
          })}

          {Object.entries(BOXES).map(([id, b]) => {
            const involved = visible.some((h) => h.from === id || h.to === id);
            const off = id === "queue" && !usesQueue;
            const down = id === "primary" && state?.primaryDown;
            return (
              <g key={id} className={`box ${involved ? "involved" : ""} ${off ? "off" : ""} ${down ? "down" : ""}`}>
                <rect x={b.x} y={b.y} width={b.w} height={b.h} rx="10" />
                <text x={b.x + 12} y={b.y + 22} className="box-title">
                  {id === "client" ? actor : b.label}
                </text>
                {id === "checkapi" && state && (
                  <text x={b.x + 12} y={b.y + 42} className="box-sub">
                    {byId[state.strategy].short}
                  </text>
                )}
                {id === "primary" && state && (
                  <>
                    <text x={b.x + 12} y={b.y + 44} className={`box-sub ${down ? "down-sub" : ""}`}>
                      {down ? "✕ UNAVAILABLE" : "source of truth"} · rev {state.primaryRev}
                    </text>
                    <text x={b.x + 12} y={b.y + 64} className="box-sub">
                      doc:42 version token: ver#{state.docRev["doc:42"] ?? 0}
                    </text>
                    <text x={b.x + 12} y={b.y + 84} className="box-sub">
                      doc:7 version token: ver#{state.docRev["doc:7"] ?? 0}
                    </text>
                  </>
                )}
                {id === "cache" && state && (
                  <>
                    {state.cache.length === 0 && (
                      <text x={b.x + 12} y={b.y + 46} className="box-sub">
                        empty
                      </text>
                    )}
                    {state.cache.slice(0, 4).map((e, i) => (
                      <text key={e.subject + e.object} x={b.x + 12} y={b.y + 46 + i * 18} className={`box-sub entry ${isStale(e) ? "stale-entry" : ""}`}>
                        {who(e.subject)}→{e.object} {e.decision} @rev {e.rev}
                        {isStale(e) ? " ⚠ stale" : ""}
                      </text>
                    ))}
                  </>
                )}
                {id === "queue" && (
                  <>
                    {off && (
                      <text x={b.x + 12} y={b.y + 46} className="box-sub">
                        not used by this strategy
                      </text>
                    )}
                    {usesQueue && queued.length === 0 && (
                      <text x={b.x + 12} y={b.y + 46} className="box-sub">
                        empty
                      </text>
                    )}
                    {usesQueue &&
                      queued.slice(0, 4).map((m, i) => (
                        <text key={m.id} x={b.x + 12} y={b.y + 46 + i * 18} className="box-sub entry queued-entry">
                          {i === 0 ? "▶ " : "  "}#{m.id} {who(m.subject)}→{m.object} {m.op === "delete" ? "revoke" : "grant"} · queued
                        </text>
                      ))}
                    {usesQueue && queued.length > 4 && (
                      <text x={b.x + 12} y={b.y + 46 + 4 * 18} className="box-sub">
                        +{queued.length - 4} more
                      </text>
                    )}
                  </>
                )}
              </g>
            );
          })}

          {[...byEdge.entries()].map(([key, items]) => {
            const pos = LABEL_POS[key];
            return (
              <g key={key}>
                {items.map((it, j) => (
                  <text key={it.n} x={pos.x} y={pos.y + j * 16} textAnchor={pos.anchor} className={`hop-label k-${it.kind ?? "none"}`}>
                    <tspan className="hop-n">{it.n}</tspan> {it.text}
                  </text>
                ))}
              </g>
            );
          })}
        </svg>
      </div>
      <figcaption className="diagram-foot">
        {hops.length === 0 ? (
          <span className="muted">Run a step to trace a request through the system.</span>
        ) : (
          <span>
            Hop {Math.min(shown, hops.length)}/{hops.length}
            {current && <> · {current.label}</>}
          </span>
        )}
        <span className="spacer" />
        {usesQueue && onDeliver && (
          <button className="deliver-btn" onClick={onDeliver} disabled={busy || queued.length === 0}>
            Deliver next invalidation ({queued.length} queued)
          </button>
        )}
        {hops.length > 0 && (
          <>
            <strong className="total">critical path: {fmtMs(total)} ms</strong>
            <button onClick={() => setShown(0)}>Replay</button>
          </>
        )}
      </figcaption>
    </figure>
  );
}
