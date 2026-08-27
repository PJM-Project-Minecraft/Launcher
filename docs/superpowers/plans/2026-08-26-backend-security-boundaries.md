# Backend Security Boundaries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fail closed at backend request, credential, session, TOTP, and entropy boundaries.

**Architecture:** Put enforcement at shared pre-consumer boundaries: Fiber transport ingestion, `Config.Validate`, atomic repository updates, Yggdrasil service issuance, and anti-cheat nonce ownership. Route handlers retain their public JSON contracts.

**Tech Stack:** Go 1.26, Fiber v3.3, GORM, SQLite/PostgreSQL tests.

**Spec:** `docs/superpowers/specs/2026-08-26-security-remediation-design.md`

## Global Constraints

- Production secrets are 64 lowercase hex characters and pairwise distinct.
- Development defaults remain available.
- No session/store mutation occurs after entropy failure.
- TOTP replay protection is atomic.
- Existing user changes in `backend/internal/policy/rules.md` and `docker-compose.yml` are preserved.

---

### Task 1: Production secret validation and HS256 pinning

**Files:**
- Modify: `backend/internal/config/config.go:223-266`
- Modify: `backend/internal/config/config_test.go`
- Modify: `backend/internal/auth/service.go:115-145`
- Modify: `backend/internal/auth/middleware_test.go`

**Interfaces:**
- Produces: `validateProductionSecret(name, value string) error`
- Preserves: `Config.ValidateBot` excludes delivery signing requirements.

- [ ] Add table tests rejecting 63 chars, uppercase hex, non-hex, repeated single-nibble, whitespace, and pairwise reuse; accept four independent 64-lowercase-hex values.
- [ ] Run `cd backend && go test ./internal/config`; confirm the new cases fail because current validation accepts them.
- [ ] Implement `validateProductionSecret`, invoke it for JWT, anti-cheat, game, site, and enabled P5 secrets, then enforce pairwise distinction.
- [ ] Add a JWT test whose HS384 token is rejected despite a correct secret.
- [ ] Run the JWT test and confirm it fails under the current generic HMAC method check.
- [ ] Pin parsing to `jwt.SigningMethodHS256` and rerun `go test ./internal/auth ./internal/config`.

### Task 2: Entropy-safe Yggdrasil issuance and refresh

**Files:**
- Modify: `backend/internal/yggdrasil/service.go`
- Modify: `backend/internal/yggdrasil/handler.go`
- Modify: `backend/internal/yggdrasil/handler_test.go`
- Modify: `backend/internal/yggdrasil/flow_test.go`

**Interfaces:**
- Produces: `randomToken(io.Reader) (string, error)` and service-owned entropy reader.
- Changes: `IssueSession` returns `(Session, error)`; refresh generation occurs before store mutation.

- [ ] Add tests for reader error, short read, second-token failure, and refresh failure; assert no session is stored and an existing refresh token stays valid.
- [ ] Run focused tests and observe the predictable/fall-through behavior fail the assertions.
- [ ] Inject `crypto/rand.Reader`, use `io.ReadFull`, return 32 lowercase hex characters, and propagate errors as temporary 500 responses without token fields.
- [ ] Update all direct callers and rerun `go test ./internal/yggdrasil`.

### Task 3: Atomic TOTP step-up and non-denying account throttle

**Files:**
- Modify: `backend/internal/repo/repo.go`
- Modify: `backend/internal/auth/local_provider.go`
- Modify: `backend/internal/auth/local_provider_test.go`
- Modify: `backend/internal/bot/link.go`
- Modify: `backend/internal/bot/lifecycle.go`
- Modify: `backend/internal/bot/totp.go`
- Modify: `backend/internal/models/bot.go`
- Add/modify tests: `backend/internal/bot/*_test.go`, `backend/internal/repo/integration_test.go`

**Interfaces:**
- Produces: `repo.ConsumeTOTPStep(ctx, db, userID string, step int64) (bool, error)`.
- Produces: Telegram dialogue state `FlowLinkTotp` carrying the target user and a consumed-step marker before chat OTP.

- [ ] Add a concurrent test where two consumers submit the same TOTP step and exactly one succeeds.
- [ ] Add login test: after 30 failures, correct password plus valid TOTP succeeds; invalid password remains throttled.
- [ ] Add bot tests: TOTP-enabled link cannot reach chat OTP without current TOTP, replay fails, enabling TOTP between password and final bind invalidates the grant.
- [ ] Run the focused tests and confirm current pre-bcrypt lock and password-only bot flow fail them.
- [ ] Implement conditional GORM update with `totp_last_step < step`, use it in primary login and high-risk bot flows, and move hard account-lock rejection to the invalid-password outcome.
- [ ] Refetch user/TOTP state before final `BindTelegram` and require the matching step-up marker.
- [ ] Rerun `go test ./internal/auth ./internal/bot ./internal/repo`.

### Task 4: Anti-cheat claim identity binding

**Files:**
- Modify: `backend/internal/yggdrasil/store.go`
- Modify: `backend/internal/anticheat/service.go`
- Modify: `backend/internal/anticheat/service_test.go`
- Modify: `backend/internal/anticheat/spoof_test.go`

**Interfaces:**
- Produces: a nonce-owner lookup returning server-side UUID and normalized login.
- Changes: `VerifySessionToken` and `Confirm` reject signed claims that do not match the nonce owner before side effects.

- [ ] Add tests signing victim UUID/login over an attacker's live nonce; assert confirm, detect/session verification, revocation clearing, and screenshot authorization fail.
- [ ] Run focused anti-cheat tests and observe current signature-plus-active-nonce acceptance.
- [ ] Add the store lookup and constant normalization, enforce identity equality in the common verifier and Confirm path.
- [ ] Rerun `go test ./internal/anticheat ./internal/yggdrasil`.

### Task 5: Transport-level body budgets

**Files:**
- Modify: `backend/cmd/server/main.go`
- Add: `backend/internal/middleware/body_budget.go`
- Add: `backend/internal/middleware/body_budget_test.go`
- Modify: `backend/internal/launcherrelease/handler.go`
- Modify: `backend/internal/launcherrelease/handler_test.go`
- Modify: `backend/internal/anticheat/handler.go`

**Interfaces:**
- Default JSON budget: 1 MiB.
- Screenshot budget: existing `screenshotMaxBodySize`.
- Launcher-release multipart budget: 512 MiB, streamed/spooled rather than pre-parsed into memory.

- [ ] Add raw-listener tests for oversized Content-Length and chunked JSON; handler must not execute and response is 413.
- [ ] Add release multipart tests at a small fixture limit proving accepted files are streamed and an over-limit stream fails without a partial published release.
- [ ] Run tests and confirm the current 512 MiB application budget fails the JSON cases.
- [ ] Enable request streaming and disable multipart preparse; install a bounded reader before JSON bind and implement explicit bounded multipart parsing for both release aliases.
- [ ] Preserve screenshot and release ceilings, drain/close request streams safely, and rerun middleware/auth/Yggdrasil/release/screenshot tests.

### Task 6: Backend verification

- [ ] Run `cd backend && gofmt -w` only on changed Go files.
- [ ] Run `cd backend && go test ./internal/config ./internal/auth ./internal/repo ./internal/bot ./internal/yggdrasil ./internal/anticheat ./internal/middleware ./internal/launcherrelease`.
- [ ] Run `cd backend && go vet ./... && go test ./...`.
- [ ] Run `git diff --check` and inspect the backend diff for unrelated changes.
