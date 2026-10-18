import type { CheckResult, CompareRow, DeliverResult, State, Strategy, SuiteResult, WriteResult } from "./types";

const SESSION_KEY = "authz-lab-session";
let memorySession = "";

// One isolated demo session per browser, so reviewers never share state.
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

// The same operations exist on the main demo (/api/v1) and the queue simulation (/api/v1/sim).
function world(prefix: string) {
  return {
    state: () => call<State>("GET", `${prefix}/state`),
    reset: () => call<State>("POST", `${prefix}/reset`),
    check: (subject: string, object: string) => call<CheckResult>("POST", `${prefix}/check`, { subject, object }),
    grant: (subject: string, object: string) => call<WriteResult>("POST", `${prefix}/grant`, { subject, object }),
    revoke: (subject: string, object: string) => call<WriteResult>("POST", `${prefix}/revoke`, { subject, object }),
    deliver: () => call<DeliverResult>("POST", `${prefix}/deliver`),
  };
}

export const api = {
  ...world("/api/v1"),
  setStrategy: (strategy: Strategy) => call<State>("POST", "/api/v1/strategy", { strategy }),
  compare: () => call<{ rows: CompareRow[] }>("GET", "/api/v1/compare"),
  runTests: () => call<SuiteResult>("POST", "/api/v1/tests/run"),
  sim: {
    ...world("/api/v1/sim"),
    otherWrites: (count: number) => call<WriteResult[]>("POST", "/api/v1/sim/other-writes", { count }),
  },
};
