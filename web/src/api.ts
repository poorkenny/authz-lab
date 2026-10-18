import type { CheckResult, Mode, State, SuiteResult, WriteResult } from "./types";

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

export const api = {
  state: () => call<State>("GET", "/api/v1/state"),
  reset: () => call<State>("POST", "/api/v1/reset"),
  setMode: (mode: Mode) => call<State>("POST", "/api/v1/mode", { mode }),
  check: (subject: string, object: string) => call<CheckResult>("POST", "/api/v1/check", { subject, object }),
  grant: (subject: string, object: string) => call<WriteResult>("POST", "/api/v1/grant", { subject, object }),
  revoke: (subject: string, object: string) => call<WriteResult>("POST", "/api/v1/revoke", { subject, object }),
  runTests: () => call<SuiteResult>("POST", "/api/v1/tests/run"),
};
