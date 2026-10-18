import type { CheckResult, CompareRow, State, Strategy, SuiteResult, WriteResult } from "./types";

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

type Pair = { subject: string; object: string; waitMs?: number };

export const api = {
  state: () => call<State>("GET", "/api/v1/state"),
  reset: () => call<State>("POST", "/api/v1/reset"),
  setStrategy: (strategy: Strategy) => call<State>("POST", "/api/v1/strategy", { strategy }),
  check: (p: Pair) => call<CheckResult>("POST", "/api/v1/check", p),
  grant: (p: Pair) => call<WriteResult>("POST", "/api/v1/grant", p),
  revoke: (p: Pair) => call<WriteResult>("POST", "/api/v1/revoke", p),
  compare: () => call<{ rows: CompareRow[] }>("GET", "/api/v1/compare"),
  runTests: () => call<SuiteResult>("POST", "/api/v1/tests/run"),
};
