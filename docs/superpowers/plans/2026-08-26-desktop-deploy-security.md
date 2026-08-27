# Desktop and Deploy Security Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove plaintext bearer persistence and make production deployment validate its effective Compose model before restart.

**Architecture:** The desktop keeps keyring-first persistence and falls back only to process memory. A standalone preflight validates rendered production configuration without printing secrets and is called remotely by `deploy.sh` before Compose mutation.

**Tech Stack:** Rust/Tauri, Bash, Python 3 standard library, Docker Compose.

**Spec:** `docs/superpowers/specs/2026-08-26-security-remediation-design.md`

## Global Constraints

- No plaintext JWT remains in active, pending, or backup settings files.
- Non-secret settings and atomic recovery remain compatible.
- Deployment validation occurs before `docker compose up` and never prints secret values.
- Existing `LAUNCHER_DOWNLOAD_URL` user change in `docker-compose.yml` is preserved.

---

### Task 1: Memory-only fallback when keyring is unavailable

**Files:**
- Modify: `src-tauri/src/main.rs:4618-4760`
- Modify tests in: `src-tauri/src/main.rs:5400-5500`

**Interfaces:**
- `save_token` persists only to a trusted keyring and returns a status indicating durable versus memory-only storage.
- `load_token` never authenticates from `settings.auth_token`; it clears legacy plaintext during settings recovery/write.

- [ ] Add tests proving keyring failure does not serialize a token, legacy plaintext is cleared, and non-secret settings survive recovery.
- [ ] Run `cd src-tauri && cargo test --locked keyring` and confirm current fallback fails the assertions.
- [ ] Remove plaintext writes/reads, keep the live token in application state, and scrub active/new/old settings representations through the atomic writer.
- [ ] Rerun focused Rust tests and `cargo test --locked`.

### Task 2: Fail-closed production Compose preflight

**Files:**
- Add: `scripts/prod/compose-security-preflight.py`
- Add: `scripts/prod/test-compose-security-preflight.py`
- Modify: `deploy.sh`
- Modify carefully: `docker-compose.yml`

**Interfaces:**
- Script consumes rendered JSON from `docker compose config --format json` and returns only named invariant failures.
- Required: production app env, four strong distinct secrets, PostgreSQL DSN, loopback published postgres/server ports, and persistent `/app/storage` mount.

- [ ] Add fixture tests for missing env/override, development mode, weak/reused secrets, wildcard IPv4/IPv6 binds, missing storage, and one valid rendered model.
- [ ] Run the Python tests and confirm the validator is absent/failing.
- [ ] Implement the validator using only Python standard library; redact all environment values.
- [ ] Change base host-bind defaults to `127.0.0.1:5432` and `127.0.0.1:8080` without changing container `SERVER_ADDR`.
- [ ] Invoke the remote preflight after fetch/reset and before `docker compose up`; require `.env` and override existence.
- [ ] Run Python tests, `bash -n deploy.sh`, and `docker compose config -q`.

### Task 3: Desktop/deploy verification

- [ ] Run `cd src-tauri && cargo fmt --check && cargo test --locked`.
- [ ] Run `python3 scripts/prod/test-compose-security-preflight.py`.
- [ ] Run `bash -n deploy.sh scripts/prod/*.sh` for tracked production scripts.
- [ ] Run `git diff --check` and inspect overlaps with the pre-existing Compose change.
