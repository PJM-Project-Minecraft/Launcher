# Launcher Security Remediation Design

## Goal

Close the source-backed security findings from scan `abbe4b5e-b503-44ae-bac7-2466d6cbb132` without weakening current authorization, signed delivery, or development workflows. The hostile-player model includes control of the launcher process, JVM, local files, and network; client evidence is therefore telemetry unless it is backed by a server-authoritative lease or a future platform attestation issuer.

## Binding decisions

- Production capability secrets use exactly 64 lowercase hexadecimal characters, generated as 32 random bytes. Existing production values must rotate before the hardened server is deployed.
- JWT verification accepts HS256 only.
- The production deploy path requires `.env`, `docker-compose.override.yml`, `APP_ENV=production`, loopback host bindings, strong distinct secrets, PostgreSQL, and persistent `/app/storage` before restart.
- Public JSON ingestion is limited before decoding. Large launcher-release multipart input and screenshot input use explicit bounded streaming paths.
- Entropy failure never creates or mutates a Yggdrasil session.
- Enabled TOTP is atomically consumed for launcher login and Telegram recovery-channel enrollment.
- A correct password is never rejected solely because attackers exhausted a per-account failure counter. Invalid attempts remain constrained by existing IP/chat controls.
- A forged anti-cheat claim must match the server-side nonce owner before any report, confirm, revocation clearing, screenshot, or heartbeat side effect.
- Plaintext JWT persistence is removed. Keyring failure leaves the token memory-only, and legacy `settings.json.auth_token` is cleared without being trusted.
- The unsigned screenshot completion channel is removed. Completion requires the native pixel HMAC and stored captures are explicitly client-untrusted telemetry. Pending/fail remain soft client telemetry and cannot create a trusted capture.
- P5 gains a versioned, server-authoritative connection lease. The game server renews every 30 seconds and disconnects after 180 seconds without a valid renewal. Old protocol remains non-enforcing only for a bounded rollout configuration; production completion requires enabling v2.
- Current client proof is named protocol presence, not cryptographic executable attestation. Hardware-backed executable attestation is deliberately outside this patch.

## Compatibility

- Development keeps SQLite, derived development secrets, and loopback-accessible Compose services.
- Existing auth/Yggdrasil JSON contracts and status codes remain except oversized requests return 413 before decode.
- Launcher-release uploads retain both legacy and v2 route aliases and the 512 MiB ceiling.
- Valid signed native screenshot captures remain accepted; capture failures remain soft telemetry rather than automatic bans.
- Multiple same-name Yggdrasil sessions remain supported; leases bind to nonce/session identity rather than login alone.
- Existing non-secret launcher settings retain atomic recovery semantics.

## Verification

Every behavior change follows red-green TDD. Focused Go/Rust/Java checks run first, followed by `./dev-check --full`. Runtime gameplay, production secret rotation, mandatory client rollout, and external dashboard labeling remain separate acceptance gates and must not be claimed from source tests.
