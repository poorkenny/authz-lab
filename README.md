# Stale Permissions: Three Strategies, Three Trade-offs

**Goal: a showcase that compares strategies for stopping a permission cache from serving stale ALLOWs after access is revoked. For each one it shows what it fixes, what it can't promise, and what it costs.**

[![CI](https://github.com/poorkenny/authz-lab/actions/workflows/ci.yml/badge.svg)](https://github.com/poorkenny/authz-lab/actions/workflows/ci.yml)
[![Deploy to Render](https://render.com/images/deploy-to-render-button.svg)](https://render.com/deploy?repo=https://github.com/poorkenny/authz-lab)

![Goal banner and the three strategies](docs/images/showcase-strategies.png)

```
client → Check API → permission cache → primary store (SQLite)
admin  ───────────────────────────────→ primary store (grant / revoke)
```

Permission checks are cached for speed. When an admin revokes access in SQLite, any cached ALLOW for that user becomes wrong. How long can it keep being served?

| | 1 · TTL cache (naive) | 2 · TTL + invalidation on write | 3 · Revision-aware fallback |
|---|---|---|---|
| **How** | Cache until the TTL expires; writes don't touch the cache | After each SQLite commit, an invalidation event goes onto a delivery queue; the cache drops the entry when it is delivered | Entries stamped with their revision; requests carry the doc's revision token; older entries fall back to SQLite |
| **Stale window** | Up to the full TTL (5 min here) | From the commit until the event is delivered, including any backlog ahead of it | **None** |
| **Read latency** | 1.2 ms hit · 16.3 ms miss | 1.2 ms hit · 16.3 ms miss after a delivery | 1.21 ms hit · 16.31 ms fallback, once per change |
| **Write cost** | None | Enqueue an invalidation event per write (0.3 ms) | Bump the doc's token in the same SQLite transaction |
| **Needs** | Nothing | A delivery queue / pub-sub to every cache node | Revision tokens propagated with requests (like Zanzibar zookies) |
| **Guarantee** | ✕ None | ! Best effort: mitigates, can't promise freshness | ✓ Every decision reflects at least the doc's latest ACL change |

---

## Interactive demonstration

The same five-step story runs under the strategy you pick. **Invalidation events are delivered only when you click**, so every run is deterministic:

1. **Alice opens doc:42**: cache miss → SQLite → ALLOW cached at rev 3.
2. **Admin revokes Alice**: committed in SQLite at rev 4. With the invalidation strategy, event #1 is queued.
3. **Alice reads before delivery**: the event is still in the queue.
4. **Deliver the invalidation event**: manual; delivers the head of the queue to the cache.
5. **Alice reads after delivery**.

The data-flow diagram replays every request hop by hop, **with the latency of each hop**. It includes the invalidation queue node, which lists the queued events and has a **Deliver next invalidation** button.

![Invalidation strategy, step 3: event #1 still queued, stale ALLOW served from the cache](docs/images/showcase-race.png)

The page also runs the story for all three strategies side by side:

![Same story, all three strategies](docs/images/showcase-compare.png)

| Strategy | Read before delivery | Read after delivery | Verdict |
|---|---|---|---|
| TTL cache | ✕ ALLOW (cache, 1.2 ms) | ✕ ALLOW (cache, 1.2 ms) | stale until TTL |
| TTL + invalidation | ✕ ALLOW (cache, 1.2 ms) | ✓ DENY (SQLite, 16.3 ms) | stale until the event is delivered |
| Revision-aware | ✓ DENY (SQLite fallback, 16.3 ms) | ✓ DENY (cache, 1.2 ms) | always fresh |

## Simulation: read-after-write inconsistency in the invalidation queue

A separate world (always the invalidation strategy) where you control the delivery queue directly: Alice reads, admin revokes / re-grants Alice, unrelated writes (+1 / +3) that queue their own events, and **Deliver next invalidation**, one event at a time in FIFO order. Every read records where Alice's invalidation sat in the queue at that moment.

**Load scenario** puts Alice's revoke behind 3 queued events:

![Queue simulation: Alice's invalidation #4 behind 3 others; her reads stay stale](docs/images/showcase-queue-sim.png)

| Alice's read | Cache answered | SQLite says | Alice's invalidation at that moment |
|---|---|---|---|
| before any delivery | ✕ ALLOW | DENY | #4 queued at position 4 of 4 |
| after 1 delivery | ✕ ALLOW | DENY | #4 queued at position 3 of 3 |
| after 3 deliveries | ✕ ALLOW | DENY | #4 queued at position 1 of 1 |
| after 4 deliveries | ✓ DENY | DENY | none queued (read from SQLite) |

## Correctness tests

**Run correctness tests** on the page executes `demo.RunSuite()` on the server. CI runs the same code via `go test ./internal/demo`.

![Correctness tests: 9/9 passed](docs/images/showcase-tests.png)

| Test | Strategy | What it proves |
|---|---|---|
| TTL cache serves a stale ALLOW until the TTL expires | TTL | the problem is real (passes when reproduced) |
| A read right after the write is stale while the event is queued | Invalidation | read-after-write inconsistency |
| Delivering the event fixes the cache | Invalidation | it mitigates |
| A queue backlog stretches the inconsistency | Invalidation | stale through 3 deliveries, fresh only after her own |
| Revision-aware is correct before any delivery | Revision-aware | no invalidation needed |
| Revision-aware pays the fallback once, then hits the cache again | Revision-aware | the cost is bounded |
| Revision-aware decisions always match SQLite | property | 30 seeds × 400 random ops: **0 wrong** |
| Invalidation mitigates, but cannot eliminate, stale decisions | property | far fewer mistakes than TTL, every one while the user's event was queued |
| The same workloads catch the TTL cache | property | the property test can actually fail |

Property-test results (identical seeded workloads of grants, revokes, deliveries and checks):

| | TTL | Invalidation | Revision-aware |
|---|---|---|---|
| wrong decisions | 2,579 | 361 (all while the user's event was queued) | **0** |
| cache hit rate | 98.0% | 90.6% | 78.1% |
| mean check latency | 1.50 ms | 2.62 ms | 4.51 ms |

Invalidation looks cheaper than revision-aware because it keeps serving cached entries until their event is delivered. Those are exactly the reads where it was wrong. Revision-aware pays an extra SQLite round trip right after each change and is never wrong.

---

## Run locally

Requirements: Go 1.25+, Node 22+. SQLite is embedded (pure-Go driver `modernc.org/sqlite`, no CGO), so nothing else needs installing.

```bash
make build && ./bin/authz-lab   # UI + API on http://localhost:8080
make test                        # go vet, Go tests (the correctness suite), TS typecheck
```

For development with hot reload: `make dev-api` (backend on :8080), then `make dev-ui` (Vite on :5173, proxies `/api`). With Docker: `make docker-run`, then open http://localhost:10000.

## API

All endpoints accept an `X-Lab-Session` header. Each session has two worlds, each with its own in-memory SQLite database, cache and invalidation queue: the main demo (`/api/v1/...`) and the queue simulation (`/api/v1/sim/...`, always the invalidation strategy).

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/state` | strategy, SQLite revision, tuples, revision tokens, cache entries, queued + recently delivered invalidations |
| `POST /api/v1/strategy {strategy: "ttl" \| "invalidation" \| "revision_aware"}` | switch strategy (starts a fresh world) |
| `POST /api/v1/check {subject, object}` | check `view`. Returns decision, SQLite's answer, served-by, revisions, the user's queued invalidation (position in queue) and **hops with per-hop latency** |
| `POST /api/v1/grant` · `POST /api/v1/revoke {subject, object}` | change access in SQLite (queues an invalidation event under the invalidation strategy) |
| `POST /api/v1/deliver` | deliver the invalidation event at the head of the queue |
| `GET /api/v1/compare` | run the five-step story under every strategy |
| `POST /api/v1/reset` | restore the starting state |
| `GET /api/v1/audit?stale=true` | audit log of writes and checks |
| `POST /api/v1/tests/run` | run the correctness suite |
| `/api/v1/sim/{state,reset,check,grant,revoke,deliver}` | the same operations on the queue-simulation world |
| `POST /api/v1/sim/other-writes {count}` | commit 1–10 unrelated writes, each queueing its own invalidation event |

```bash
H='X-Lab-Session: demo'; B=http://localhost:8080
curl -s -H "$H" -X POST $B/api/v1/strategy -d '{"strategy":"invalidation"}'
curl -s -H "$H" -X POST $B/api/v1/check  -d '{"subject":"user:alice","object":"doc:42"}'   # ALLOW, cached
curl -s -H "$H" -X POST $B/api/v1/revoke -d '{"subject":"user:alice","object":"doc:42"}'   # invalidation #1 queued
curl -s -H "$H" -X POST $B/api/v1/check  -d '{"subject":"user:alice","object":"doc:42"}'   # stale ALLOW
curl -s -H "$H" -X POST $B/api/v1/deliver                                                  # deliver #1
curl -s -H "$H" -X POST $B/api/v1/check  -d '{"subject":"user:alice","object":"doc:42"}'   # DENY
```

---

## Deploy to Render (free tier)

The repo ships a [Render Blueprint](render.yaml) that builds the [Dockerfile](Dockerfile) (the UI is embedded in the Go binary, so it's a single web service) on the free plan with a `/healthz` health check.

### Option A: one click

1. Click **Deploy to Render** above (or open `https://render.com/deploy?repo=https://github.com/poorkenny/authz-lab`).
2. Sign in to Render (signing in with GitHub is easiest) and authorize access to the repo if asked.
3. Review the service from `render.yaml` (**authz-lab**, Docker, **Free**) and click **Apply** / **Deploy Blueprint**.
4. Wait for the first build (a few minutes). The URL appears on the dashboard, e.g. `https://authz-lab.onrender.com`.

### Option B: Blueprint from the dashboard

1. In the [Render dashboard](https://dashboard.render.com) choose **New → Blueprint**.
2. Connect GitHub and select `poorkenny/authz-lab`. Render detects `render.yaml`.
3. Click **Apply**.

### Option C: manual web service

1. **New → Web Service** → select `poorkenny/authz-lab`.
2. **Language**: Docker · **Branch**: `main` · **Dockerfile path**: `./Dockerfile` · **Instance type**: Free.
3. **Advanced → Health Check Path**: `/healthz`. No environment variables are needed (Render injects `PORT`).
4. Click **Deploy Web Service**.

### Verify

```bash
APP=https://<your-service>.onrender.com
curl -s $APP/healthz                                         # {"ok":true,...}
curl -s -X POST $APP/api/v1/tests/run | head -c 200          # {"passed":true,...}
```

### Updates and free-tier behaviour

- `autoDeploy: true`: every push to `main` redeploys (after CI passes on the PR).
- The free instance **sleeps after ~15 min idle**, and the first request then takes about a minute. Open the URL shortly before a review.
- **No persistent disk**: each session's SQLite databases live in memory and reset on restart, by design.
- Safeguards for public access: per-browser sessions (max 500, 30 min idle expiry, database closed on expiry), 10 req/s rate limit per session, 16 KB request bodies, and CSP / frame / nosniff headers.

Free-tier limits change; check [render.com/pricing](https://render.com/pricing).

---

## Project layout

```
cmd/server/        HTTP server: API + embedded UI
internal/demo/     the three strategies, the invalidation delivery queue (queue.go),
                   per-hop latency, and the correctness suite (suite.go, suite_test.go)
internal/store/    SQLite primary store: tuples, changelog (= revisions), doc revision tokens
internal/audit/    audit log
internal/api/      REST handlers, sessions, rate limiting
web/               React + TypeScript single-page UI (Vite), embedded via web/embed.go
render.yaml        Render Blueprint
```
