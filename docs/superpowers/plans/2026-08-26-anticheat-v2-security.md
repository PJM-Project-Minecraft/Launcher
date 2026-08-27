# Anti-Cheat v2 Security Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace indefinite client-reporter trust with bounded server-side connection leases and remove screenshot paths that imply authenticity unavailable under the hostile-host model.

**Architecture:** The backend issues a nonce-bound lease status, while the game-server mod renews each online connection and enforces a 180-second local deadline even during backend outages. Screenshot endpoints accept only the signed native capture channel and record captures as client-untrusted telemetry.

**Tech Stack:** Go, NeoForge Java, Gradle, GORM.

**Spec:** `docs/superpowers/specs/2026-08-26-security-remediation-design.md`

## Global Constraints

- Poll interval is 30 seconds; outage grace is 180 seconds.
- Leases bind to a concrete nonce/session, never login alone.
- Reconnect cannot let an old lease timer kick the new connection.
- Screenshot failure remains soft telemetry; no automatic ban is added.
- Current proof is protocol presence, not executable attestation.

---

### Task 1: Native-only untrusted screenshot telemetry

**Files:**
- Modify: `backend/internal/anticheat/handler.go`
- Modify: `backend/internal/anticheat/service.go`
- Modify: `backend/internal/models/anticheat.go`
- Modify: `backend/internal/database/database.go`
- Modify: `backend/internal/anticheat/screenshot_test.go`
- Modify: `backend/internal/anticheat/capture_test.go`

**Interfaces:**
- Completion requires a valid native pixel HMAC; pending/fail remain soft session telemetry.
- Screenshot JSON includes `trust: "client_untrusted"` and capture channel metadata.

- [ ] Add tests rejecting screenshot-token JPEG completion and swapped ID signatures, and accepting a valid signed native PNG.
- [ ] Run focused tests and observe legacy paths remain accepted.
- [ ] Remove the unsigned branch and launch-token fallback, persist trust/channel metadata, retain existing caps and nonce ownership.
- [ ] Rerun `go test ./internal/anticheat`.

### Task 2: Backend connection lease protocol

**Files:**
- Modify: `backend/internal/anticheat/p5.go`
- Modify: `backend/internal/anticheat/service.go`
- Modify: `backend/internal/yggdrasil/store.go`
- Modify: `backend/internal/anticheat/p5_test.go`

**Interfaces:**
- Versioned P5 request supplies player UUID, login, and nonce/session ID.
- Response supplies `valid` plus remaining time derived from the last agent heartbeat; renewal requires a live matching server-side session.

- [ ] Add tests for live matching session, mismatched identity, expired session, explicit revocation, and dual-reporter silence.
- [ ] Run tests and confirm current revoke-only API cannot express validity.
- [ ] Implement the lease endpoint and shared session identity validation; keep old endpoint available only under explicit rollout configuration.
- [ ] Rerun backend anti-cheat/Yggdrasil tests.

### Task 3: Game-server lease enforcement

**Files:**
- Modify: `anticheat-neoforge/src/main/java/xyz/projectminecraft/anticheat/p5/P5ServerHandler.java`
- Modify: `anticheat-neoforge/src/main/java/xyz/projectminecraft/anticheat/p5/P5RevokePoller.java`
- Modify: `anticheat-neoforge/src/main/java/xyz/projectminecraft/anticheat/p5/P5Payloads.java`
- Modify/add Java tests under `anticheat-neoforge/src/test/java/.../p5/`

**Interfaces:**
- Poll each connection every 30 seconds.
- Store a per-connection renewal deadline; disconnect after 180 seconds without a valid renewal.

- [ ] Add deterministic clock tests for renewal, transient outage, outage beyond grace, reconnect replacement, and duplicate same-name sessions.
- [ ] Run Gradle tests and confirm current fail-open behavior fails the deadline cases.
- [ ] Implement nonce-bound lease state and local expiry enforcement; clear old state on disconnect/reconnect.
- [ ] Run the focused Java suite and `./gradlew build` using the repository wrapper or documented Gradle command.

### Task 4: Trust terminology and anti-cheat verification

**Files:**
- Modify comments/API field names where externally serialized compatibility permits in `backend/internal/anticheat` and `backend/internal/yggdrasil`.
- Modify: `docs/anticheat-deploy.md`

- [ ] Add/adjust tests so join gating is described as protocol-presence plus active lease rather than approved-binary attestation.
- [ ] Update deployment documentation with mandatory v2 rollout order and the explicit hostile-host limitation.
- [ ] Run backend and Java anti-cheat suites.

### Task 5: Anti-cheat verification

- [ ] Run `cd backend && go test ./internal/anticheat ./internal/yggdrasil`.
- [ ] Run the NeoForge module build.
- [ ] Run `git diff --check` and inspect the protocol diff for fail-open siblings.
