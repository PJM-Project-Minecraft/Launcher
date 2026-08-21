# Codex instructions

- Read `CLAUDE.md` for architecture and deployment constraints; it is the detailed project memory.
- For unfamiliar code locations, call the `qdr` MCP semantic search before broad `rg`/`find`; then read the returned line range.
- Do not overwrite existing uncommitted work. Before handing off a change, run `./dev-check` (or `./dev-check --full` for cross-component work).
