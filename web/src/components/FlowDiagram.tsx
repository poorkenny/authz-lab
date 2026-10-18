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
  primary: { x: 300, y: 270, w: 220, h: 100, label: "Primary store (SQLite)" },
  admin: { x: 20, y: 292, w: 130, h: 56, label: "Admin" },
};

const center = (b: Box) => ({ x: b.x + b.w / 2, y: b.y + b.h / 2 });
const edgeKey = (a: string, b: string) => [a, b].sort().join("|");
const INVALIDATION_EDGE = edgeKey("primary", "cache");

// Where each edge's latency labels sit.
const LABEL_POS: Record<string, { x: number; y: number; anchor: "start" | "middle" }> = {
  [edgeKey("client", "checkapi")]: { x: 225, y: 62, anchor: "middle" },
  [edgeKey("checkapi", "cache")]: { x: 474, y: 124, anchor: "start" },
  [edgeKey("checkapi", "primary")]: { x: 420, y: 200, anchor: "start" },
  [edgeKey("admin", "primary")]: { x: 225, y: 345, anchor: "middle" },
  [INVALIDATION_EDGE]: { x: 612, y: 240, anchor: "start" },
};

const EDGES: [string, string][] = [
  ["client", "checkapi"],
  ["checkapi", "cache"],
  ["checkapi", "primary"],
  ["admin", "primary"],
  ["primary", "cache"],
];

export function shortLabel(h: Hop, i: number, hops: Hop[]): string {
  const k = `${h.from}>${h.to}`;
  if (k === "client>checkapi") return "request";
  if (k === "checkapi>client") return "response";
  if (k === "cache>checkapi") return "revision check";
  if (k === "checkapi>primary") return h.kind === "fallback" ? "fallback to SQLite" : "SQLite query";
  if (k === "checkapi>cache") return hops.slice(0, i).some((p) => p.from === "checkapi" && p.to === "cache") ? "cache refresh" : "cache lookup";
  if (k === "admin>primary") return "SQLite commit";
  if (k === "primary>cache") return h.kind === "async" ? "invalidation (async)" : "no invalidation";
  return h.label;
}

export const fmtMs = (ms: number) => (ms < 0.1 && ms > 0 ? ms.toFixed(2) : ms.toFixed(1));

export const hopMs = (h: Hop) => (h.kind === "async" ? "+80 ms async" : h.kind === "none" ? "—" : `${fmtMs(h.ms)} ms`);

interface Props {
  hops: Hop[];
  state: State | null;
  actor: string;
}

export function FlowDiagram({ hops, state, actor }: Props) {
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
    list.push({ n: i + 1, text: `${shortLabel(h, i, hops).replace("revision check", "rev check")} · ${hopMs(h)}`, kind: h.kind });
    byEdge.set(key, list);
  });
  const current = visible[visible.length - 1];
  const total = visible.reduce((a, h) => a + (h.kind === "async" ? 0 : h.ms), 0);
  const pending = state?.pending ?? [];
  const inFlight = pending.length > 0;
  const isStale = (e: CacheEntry) => e.rev < (state?.docRev[e.object] ?? 0);

  // The invalidation edge reflects the live state when the last request didn't use it.
  const invalidationHop = visible.find((h) => edgeKey(h.from, h.to) === INVALIDATION_EDGE);
  const invalidationClass = invalidationHop
    ? invalidationHop.kind === "async"
      ? "edge async"
      : "edge none-edge"
    : inFlight
      ? "edge async"
      : "edge";
  if (!invalidationHop && inFlight) {
    byEdge.set(INVALIDATION_EDGE, [
      { n: 0, text: `invalidation in flight · arrives t=${pending[pending.length - 1].deliverAt} ms`, kind: "async" },
    ]);
  }

  return (
    <figure className="diagram" aria-label="Data flow with per-hop latency">
      <div className="diagram-scroll">
        <svg viewBox="0 0 900 390" role="img" aria-label="Client, Check API, permission cache, SQLite primary store and admin, with the hops of the last request">
          {EDGES.map(([a, b]) => {
            const ca = center(BOXES[a]);
            const cb = center(BOXES[b]);
            const key = edgeKey(a, b);
            const used = byEdge.get(key);
            const kinds = used?.map((u) => u.kind) ?? [];
            const cls =
              key === INVALIDATION_EDGE
                ? invalidationClass
                : used
                  ? `edge used ${kinds.includes("stale") ? "k-stale" : kinds.includes("fallback") ? "k-fallback" : ""}`
                  : "edge";
            const active = current && edgeKey(current.from, current.to) === key;
            return <line key={key} x1={ca.x} y1={ca.y} x2={cb.x} y2={cb.y} className={`${cls} ${active ? "active" : ""}`} />;
          })}

          {Object.entries(BOXES).map(([id, b]) => {
            const involved = visible.some((h) => h.from === id || h.to === id);
            return (
              <g key={id} className={`box ${involved ? "involved" : ""}`}>
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
                    <text x={b.x + 12} y={b.y + 44} className="box-sub">
                      source of truth · rev {state.primaryRev}
                    </text>
                    <text x={b.x + 12} y={b.y + 64} className="box-sub">
                      doc:42 revision token = {state.docRev["doc:42"] ?? 0}
                    </text>
                    <text x={b.x + 12} y={b.y + 84} className="box-sub">
                      doc:7 revision token = {state.docRev["doc:7"] ?? 0}
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
                        {e.subject.replace("user:", "")}→{e.object} {e.decision} @rev {e.rev}
                        {isStale(e) ? " ⚠ stale" : ""}
                      </text>
                    ))}
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
                    {it.n > 0 && <tspan className="hop-n">{it.n}</tspan>} {it.text}
                  </text>
                ))}
              </g>
            );
          })}
        </svg>
      </div>
      <figcaption className="diagram-foot">
        <span className="clock mono">t = {state?.now ?? 0} ms</span>
        {hops.length === 0 ? (
          <span className="muted">Run a step to trace a request through the system.</span>
        ) : (
          <>
            <span>
              Hop {Math.min(shown, hops.length)}/{hops.length}
              {current && <> · {current.label}</>}
            </span>
            <strong className="total">critical path: {fmtMs(total)} ms</strong>
            <button onClick={() => setShown(0)}>Replay</button>
          </>
        )}
      </figcaption>
    </figure>
  );
}
