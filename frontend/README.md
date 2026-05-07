# devenv-manager frontend

React + Vite + xterm.js UI for the manager. Talks to the Go backend over
REST (`/v1/*`) and a single WebSocket (`/v1/sessions/{id}/ws`).

## Scripts

| Command           | Purpose                                                 |
| ----------------- | ------------------------------------------------------- |
| `pnpm dev`        | Vite dev server with HMR (default port 5173)            |
| `pnpm build`      | Type-check + production bundle                          |
| `pnpm test`       | Vitest unit tests (jsdom)                               |
| `pnpm e2e`        | Playwright e2e tests (chromium + Pixel 5 mobile)        |
| `pnpm typecheck`  | `tsc --noEmit`                                          |
| `pnpm lint`       | ESLint                                                  |
| `pnpm format`     | Prettier                                                |

## E2E test paths

The e2e suite has two execution modes; both live in `frontend/e2e/`.

- **Hermetic (default).** `/v1/*` is intercepted via `page.route` and the
  `WebSocket` constructor is replaced with an in-page `FakeWS` that
  records frame traffic on `window.__wsFrames`. The `Terminal` component
  exposes `window.__devenvTerminalSendInput` when
  `window.__DEVENV_E2E_HOOKS === true` so the spec can drive the input
  path without going through xterm.js's keyboard handling, which is
  unreliable through synthesized events. CI runs this path.
- **Live (`E2E_REAL_BACKEND=1`).** Skips the FakeWS install. Run the
  docker-compose stack first (`make up`) and pre-pull `alpine:3.20` so
  the test starts a real session.

The mobile project uses Pixel 5 (chromium-based) instead of iPhone 12
(webkit) so a single browser install suffices in CI.

## Test hooks

The Terminal component exposes `window.__devenvTerminalSendInput(text)`
when `window.__DEVENV_E2E_HOOKS === true`. The hook is a no-op in
production builds because the flag is never set.
