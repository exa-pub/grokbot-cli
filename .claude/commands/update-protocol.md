---
description: Update grokbot-tunnel to a new Grok Bot release (download, plan, implement, review)
argument-hint: "[version]"
---

Update grokbot-tunnel to the latest Grok Bot Linux client release, or to `$ARGUMENTS` if a version is given. The current version is `Version` in `internal/client/client.go`; its unpacked bundle is in `ai.tmp/grok-bot/`. Work autonomously and stop only on real blockers.

1. **Download.** Feed: `curl -s https://api2.cursor.sh/updates/api/update/linux-x64/sand/<current>/x64/stable` returns `version`, `url` (AppImage) and `sha256hash`. If there is no newer version, say so and stop. Download into `ai.tmp/grok-bot/<version>/`, verify the sha256 and unpack it without running it: `unsquashfs -o <squashfs offset>`, then extract `app.asar` into `app/` (with `@electron/asar` or your own asar parser).
2. **Branch.** From an up-to-date `main`: `git switch -c feat/update-<version>`.
3. **What changed.** Diff the new bundle against the old one: `electron-main`, `local-exec-daemon`, `package.json`. Minified names change between builds, so compare by stable markers: proto typeNames and RPCs (`python3 -I scripts/extract-proto.py <out> <dist>/local-exec-daemon/main.cjs <dist>/electron-main/*.cjs` for both versions, then diff `<out>/all/`), string literals, headers, Hello/capabilities, the auth flow, limits and error texts. Find the functions used by `scripts/compat-reference.mjs` again by their bodies.
4. **Plan.** Write `ai.tmp/update-protocol/<version>/PLAN.md`: which changes affect our CLI (protocol, auth, exec/read/ls/upload/download, headers, version), what to do about each and how to verify it. List UI and desktop-only changes separately as not applicable.
5. **Implement.** Carry out the plan: bump `Version`, update `api/proto` and run `make proto`, refresh `testdata/compat/<version>/` (fix the markers in `compat-reference.mjs`, pass it the new `dist` path), tests and code. `make lint test` and `make proto-check` must pass.
6. **Review.** Re-check the implementation against the canon in the new binary: read the bundle code, not the plan. Write `ai.tmp/update-protocol/<version>/REVIEW.md` with every discrepancy: its location in the bundle, its location in our code, severity (high/medium/low) and resolution.
7. **Fix.** Fix the high and medium discrepancies, rerun the checks and record in REVIEW.md what was fixed and what was deliberately left as is.

Finish with a short report: old→new version, key changes, check status and what remains. Do not push.
