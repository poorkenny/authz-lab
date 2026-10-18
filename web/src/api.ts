import type { AuditRecord, Config, DrillResult, SweepResult } from "./types";

const SESSION_KEY = "authz-lab-session";
let memorySession = "";

// One isolated lab session per browser, so reviewers never share state.
function sessionId(): string {
  const fresh = () => crypto.randomUUID().replace(/-/g, "");
  try {
    let id = localStorage.getItem(SESSION_KEY);
    if (!id) {
      id = fresh();
      localStorage.setItem(SESSION_KEY, id);
    }
    return id;
  } catch {
    memorySession ||= fresh();
    return memorySession;
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", "X-Lab-Session": sessionId() },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error ?? `${res.status} ${res.statusText}`);
  return data as T;
}

export const api = {
  drill: (cfg: Config) => call<DrillResult>("POST", "/api/v1/lab/drill", cfg),
  lagSweep: (req: { lagsMs?: number[]; opsPerPoint?: number; revokeRate?: number; seed?: number }) =>
    call<SweepResult>("POST", "/api/v1/lab/experiments/lag-sweep", req),
  audit: (params: Record<string, string>) =>
    call<{ records: AuditRecord[] }>("GET", "/api/v1/audit?" + new URLSearchParams(params)),
  schema: () => call<{ schema: string }>("GET", "/api/v1/schema"),
  session: sessionId,
};
