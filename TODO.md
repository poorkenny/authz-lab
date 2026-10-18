# TODO

Planned work. Each feature ships behind a flag, following the rules in [docs/FEATURES.md](docs/FEATURES.md).

## Authentication and user management

- **Status:** planned, not started
- **Planned flag:** `FEATURE_AUTH` (default `false` until released)

Today every browser is an anonymous lab session (`X-Lab-Session`). This feature adds real users:

- [ ] Sign-in for reviewers (e.g. GitHub OAuth or email magic link) instead of anonymous sessions
- [ ] User accounts and roles: **admin** (manage users, reset any world, run the correctness suite), **reviewer** (use the demo), **viewer** (read-only)
- [ ] Attribute audit-log records and test runs to the signed-in user
- [ ] Per-user rate limits and test-run quotas (on top of the server-wide one-run-at-a-time lock)
- [ ] Admin page to list, invite and remove users
- [ ] Persist users and sessions. The Render free tier has no persistent disk, so this needs a hosted database or a paid disk.
- [ ] Decide whether the public demo stays open without sign-in (read-only) when the flag is on
- [ ] Add the flag to `internal/features`, `render.yaml` and `docs/FEATURES.md`; gate the UI and API with it; add tests with the flag on and off
