import type { Config } from "../types";
import { presetDefenses } from "../procedure";

interface Props {
  config: Config;
  onChange: (c: Config) => void;
}

function Toggle(props: { label: string; hint: string; checked: boolean; onChange: (v: boolean) => void; children?: React.ReactNode }) {
  return (
    <div className="toggle">
      <label>
        <input type="checkbox" checked={props.checked} onChange={(e) => props.onChange(e.target.checked)} />
        <span>
          <strong>{props.label}</strong>
          <small>{props.hint}</small>
        </span>
      </label>
      {props.checked && props.children && <div className="toggle-params">{props.children}</div>}
    </div>
  );
}

function Num(props: { label: string; value: number; onChange: (v: number) => void; min?: number; max?: number; unit?: string }) {
  return (
    <label className="num">
      {props.label}
      <input
        type="number"
        value={props.value}
        min={props.min ?? 0}
        max={props.max}
        onChange={(e) => props.onChange(Number(e.target.value))}
      />
      {props.unit && <span className="muted">{props.unit}</span>}
    </label>
  );
}

export function Composer({ config, onChange }: Props) {
  const c = config;
  const set = (mut: (c: Config) => void) => {
    const next = structuredClone(c);
    mut(next);
    onChange(next);
  };
  const s = c.sources;
  const d = c.defenses;

  return (
    <section className="card composer" aria-label="Scenario composer">
      <div className="composer-cols">
        <fieldset>
          <legend>Sources of staleness</legend>
          <Toggle
            label="Replication lag"
            hint="replica-2's changelog is delayed"
            checked={s.replicationLag.enabled}
            onChange={(v) => set((n) => (n.sources.replicationLag.enabled = v))}
          >
            <Num label="lag" unit="ms" value={s.replicationLag.lagMs} max={60000} onChange={(v) => set((n) => (n.sources.replicationLag.lagMs = v))} />
          </Toggle>
          <Toggle
            label="Decision cache"
            hint="decisions cached per replica with a TTL"
            checked={s.decisionCache.enabled}
            onChange={(v) => set((n) => (n.sources.decisionCache.enabled = v))}
          >
            <Num label="TTL" unit="ms" value={s.decisionCache.ttlMs} max={600000} onChange={(v) => set((n) => (n.sources.decisionCache.ttlMs = v))} />
          </Toggle>
          <Toggle
            label="Indirect revocation"
            hint="revoke via group:eng → folder:7 → doc:42"
            checked={s.indirectRevocation}
            onChange={(v) => set((n) => (n.sources.indirectRevocation = v))}
          />
          <Toggle
            label="Partitioned replica"
            hint="replica-2 cut off 2 s before the revoke"
            checked={s.partition}
            onChange={(v) => set((n) => (n.sources.partition = v))}
          />
          <Toggle
            label="New enemy"
            hint="secret content written after the revoke"
            checked={s.newEnemy}
            onChange={(v) => set((n) => (n.sources.newEnemy = v))}
          />
        </fieldset>

        <fieldset>
          <legend>Defenses</legend>
          <div className="presets" role="group" aria-label="Defense presets">
            {(["naive", "tokens", "guarded", "full"] as const).map((m) => (
              <button key={m} onClick={() => onChange(presetDefenses(m, c))}>
                {m}
              </button>
            ))}
          </div>
          <Toggle
            label="Fully consistent"
            hint="every check reads the primary"
            checked={d.alwaysPrimary}
            onChange={(v) => set((n) => (n.defenses.alwaysPrimary = v))}
          />
          <Toggle label="Consistency tokens" hint="answer at ≥ the caller's token revision" checked={d.tokens} onChange={(v) => set((n) => (n.defenses.tokens = v))} />
          <Toggle
            label="Revocation watermark"
            hint="fast-lane fence; revoke acked after fan-out"
            checked={d.watermark}
            onChange={(v) => set((n) => (n.defenses.watermark = v))}
          >
            <label className="num">
              granularity
              <select
                value={d.watermarkGranularity}
                onChange={(e) => set((n) => (n.defenses.watermarkGranularity = e.target.value as "object" | "namespace"))}
              >
                <option value="object">per object (expanded)</option>
                <option value="namespace">per namespace</option>
              </select>
            </label>
          </Toggle>
          <Toggle
            label="Revision-keyed cache"
            hint="cache entries valid only at ≥ required rev"
            checked={d.revisionKeyedCache}
            onChange={(v) => set((n) => (n.defenses.revisionKeyedCache = v))}
          />
          <Toggle
            label="Fail closed"
            hint="stop serving locally when heartbeat or lag bound is violated"
            checked={d.failClosed}
            onChange={(v) => set((n) => (n.defenses.failClosed = v))}
          >
            <label className="num">
              mode
              <select value={d.failMode} onChange={(e) => set((n) => (n.defenses.failMode = e.target.value as "forward" | "deny"))}>
                <option value="forward">forward to primary</option>
                <option value="deny">deny</option>
              </select>
            </label>
            <Num label="heartbeat timeout" unit="ms" value={d.heartbeatTimeoutMs} onChange={(v) => set((n) => (n.defenses.heartbeatTimeoutMs = v))} />
            <Num label="lag bound" unit="ms" value={d.lagBoundMs} onChange={(v) => set((n) => (n.defenses.lagBoundMs = v))} />
          </Toggle>
          <Num label="Wait budget before forwarding" unit="ms" value={d.waitBudgetMs} max={5000} onChange={(v) => set((n) => (n.defenses.waitBudgetMs = v))} />
        </fieldset>
      </div>
      <details>
        <summary>Timing model</summary>
        <div className="timing">
          <Num label="healthy replica lag" unit="ms" value={c.timing.baseLagMs} onChange={(v) => set((n) => (n.timing.baseLagMs = v))} />
          <Num label="fast-lane delay" unit="ms" value={c.timing.fastLaneMs} onChange={(v) => set((n) => (n.timing.fastLaneMs = v))} />
          <Num label="primary RTT" unit="ms" value={c.timing.primaryRttMs} onChange={(v) => set((n) => (n.timing.primaryRttMs = v))} />
          <Num label="final check after revoke" unit="ms" value={c.timing.checkAfterRevokeMs} onChange={(v) => set((n) => (n.timing.checkAfterRevokeMs = v))} />
        </div>
      </details>
    </section>
  );
}
