---
status: accepted
date: 2026-10-12
decision-makers: Serghei Iakovlev
---

# Build the Dashboard as a React Application Committed and Embedded in the Binary

## Context and Problem Statement

The embedded dashboard was a single server-rendered `html/template` page that reloaded itself every five seconds. It is being rebuilt as a client-side application on the JSON API: an operational overview, an issue page, history, analytics, and health, and later interactive surfaces such as chat with agents, a workflow designer, a kanban-style tracker, authentication, and multiple users. A hand-written page cannot carry that, and the project had no frontend toolchain. Every frontend change depends on the answers recorded here: framework, language, and bundler; how built assets reach the binary; routing; how the client stays in step with the Go response types; design tokens and themes; lint and format gates; real-time delivery; and a structure that admits the later surfaces without a rewrite.

## Decision Drivers

1. **Zero dependencies for end users at runtime.** Operators download one statically linked binary and need nothing else. The dashboard works offline: no CDN, no web fonts, no external service. No CGo. Everything lives in one repository.
2. **Node is a build tool, never a prerequisite.** Node may build the dashboard. A contributor who changes only Go code, the release job, the from-source container build, and `go install module@version` never need it. The module zip that `go install` builds from contains only committed files.
3. **Go owns the meaning of Sortie state.** Operator-facing wording (status sentences, reasons, and what clears an item) is produced in Go and served by the API, and the client renders that text as received. The client may arrange served fields for display (a position in a sequence, a count, a mark, a fixed label for a served value), but it never decides what a state means, never composes a sentence about it, and never derives a reason or remedy the server did not send.
4. **Room for the later surfaces.** The graph editor, drag and drop, virtualized logs, a code editor, charts, and streamed chat rendering must each have a maintained library in the chosen ecosystem, and each screen must be addable without touching the others.
5. **Agents write most of the code.** The stack must be the one coding agents write most reliably, with a type checker they can loop against.
6. **Supply-chain exposure is bounded.** npm worms in 2026 ran from install scripts and shipped as freshly published versions, one of them with genuine build provenance. Every control that costs nothing is on.

## Considered Options

- React 19 with strict TypeScript, Vite, and npm
- Svelte 5 with Vite (with or without SvelteKit's static adapter)
- Vue 3 with Vite
- Preact, Solid, or Lit
- Vanilla TypeScript with hand-written rendering
- htmx with Go templates, no client build

## Decision Outcome

Chosen option: **React 19 with strict TypeScript, Vite, and npm, with the built output committed and embedded**, because it is the only candidate with a maintained, mainstream library for every planned surface, it is what coding agents and agent-built products write most, and committing the output keeps every Go-only path free of Node.

### Stack

| Concern | Choice | Reason |
|---|---|---|
| Framework | React 19 | Every planned surface has a first-party or market-leading package (graph editor, drag and drop, virtualization, CodeMirror bindings, charts, and streamed chat rendering). Governed by the React Foundation under the Linux Foundation. |
| Language | TypeScript, `strict` | Types catch the error class models produce most and give agents a compiler loop. TSX type-checks with the Go-native TypeScript 7 compiler; `.svelte` and `.vue` templates do not yet. |
| Bundler | Vite | Rolldown and Oxc inside, hot module replacement, content-hashed output, no Babel in the default React path. |
| Package manager | npm | Ships with the pinned Node, so the dashboard needs no tool beyond Node; Dependabot and CI caching support it natively. Its safety controls are opt-in, so the project turns them on (see Supply chain). |
| Styling | Tailwind CSS v4 | Tokens declared once in the `@theme` layer become both utilities and CSS custom properties. Compiled at build time, so no runtime style injection. |
| Primitives | Base UI, wrapped in owned, shadcn-style components | Stable since 1.0, built by the team behind Radix, and the default of shadcn's component source since mid-2026. The components are source files in the repository, so a primitive swap touches one wrapper per primitive. |
| Router | React Router, client-only library mode | The most used React router, so the one agents know best; a `/ui` basename and lazily loaded route modules are all the dashboard needs. TanStack Router's typed search parameters do not outweigh a second, less familiar routing model plus route code generation. |
| Server state | TanStack Query | Caches API responses and refetches them when the stream says they changed. It holds only what Go served, so the client has no state of its own to reconcile. |
| Client state | React state and the URL | Selection, filters, and open panels live in components or in search parameters, so views stay linkable. No global store library; a screen that later needs one (the workflow designer) adds it inside its own module. |
| Lint | Oxlint with type-aware rules, plus `tsc --noEmit` | A native binary with a handful of installed packages against roughly a hundred for ESLint with typescript-eslint; implements the React hook rules. |
| Format | Oxfmt | Same toolchain as Oxlint. Pre-1.0, so pinned exactly; Biome's formatter is the fallback if its output churns. |

Fonts are the system font stack. Icons and images ship inside the build output.

### Build output reaches the binary

The frontend project lives in `web/`. Vite writes into `internal/server/ui/dist`, a Go package directory, because a `go:embed` pattern cannot leave its package. The package embeds it with `//go:embed all:dist`; without `all:` the toolchain silently drops files whose names start with `_` or `.`, which bundlers emit. `go.mod` carries `ignore ./web/node_modules`, because npm packages ship stray Go files that `./...` would otherwise build.

The output is committed. CI installs strictly from the lockfile with `npm ci`, rebuilds, and fails if `git status --porcelain --untracked-files=all` reports anything under the output directory; `git diff` alone misses a newly emitted file that was never added. The output is marked `linguist-generated` so review diffs collapse it.

### Routing

The app is served under `/ui/` with path routing. `GET /ui/` cannot overlap `/api/v1/*`, `/metrics`, `/livez`, or `/readyz`. A root catch-all is impossible: `GET /` and `GET /{path...}` panic at registration against the method-less API patterns, and a method-less `/` would answer unknown API paths and a disabled `/metrics` with the app's HTML and status 200. The legacy page keeps `/` until it is replaced; then `/` redirects to `/ui/`.

Inside the handler, `/ui/assets/*` is a strict file server that returns 404 for a missing file, refuses directory listings, and sends `Cache-Control: public, max-age=31536000, immutable`. Every other `/ui/*` path returns `index.html` with `Cache-Control: no-cache`, an `ETag` computed once at startup (`embed.FS` has no modification time), `X-Content-Type-Options: nosniff`, and this policy:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'
```

`style-src 'self'` blocks `<style>` elements and `style` attributes written as strings, in markup or through `setAttribute`. It does not block styles set through the CSSOM, which is how React applies the `style` prop, so libraries that position elements through that prop keep working. A library that injects `<style>` elements or writes `style` attributes as strings is excluded.

### Type contract

A Go test in the server package, standard library only, walks the response types from each route's root type with `reflect`, follows `encoding/json`'s rules (a pointer, slice, or map without `omitempty` is `T | null`, because nil encodes as `null`; with `omitempty` it is `field?: T`, because nil, and an empty slice or map, is omitted; a type with its own `MarshalJSON` fails the test until it is mapped), and emits TypeScript declarations and an OpenAPI 3.2 description of the API whose schemas come from the same walk. It compares both with the committed copies, and an update flag rewrites them. A Go-only contributor who changes a response type sees a red `go test` and regenerates without Node. If the change breaks code the dashboard uses, the dashboard's type check fails, which is correct because that change needs a TypeScript edit anyway.

### Design tokens and accessibility

Color, type scale, spacing, radius, and motion tokens are defined once as CSS custom properties through Tailwind's `@theme` layer. Dark values redefine the same properties under `prefers-color-scheme: dark`, and an explicit `data-theme` attribute on the root overrides the system preference in either direction. `prefers-reduced-motion: reduce` sets the motion tokens to zero. Every text and component color pair meets WCAG 2.2 AA in both themes.

### Real-time delivery

Server-sent events, built on the Go standard library with no dependency. The stream writer clears the 10-second server write deadline for its route through `http.ResponseController`, sets a short deadline before each write so a client that stops reading is dropped instead of holding memory, sends a `heartbeat` event every 15 seconds that carries the stream's full event `id` (the last change signal's revision and the cursor of every subscribed run), sends `X-Accel-Buffering: no` so a default nginx does not buffer, and closes on server shutdown through `RegisterOnShutdown` so shutdown does not wait out its deadline. The orchestrator's observer publishes by non-blocking send, so a stream can never stall the event loop.

Change signals carry a monotonically increasing state revision as the event `id`. They are idempotent: on reconnect the client gets one signal if its revision is stale and nothing otherwise. Session events carry a sequence number per run (one dispatch) and resume from `Last-Event-ID` out of the retained events; a cursor older than retention gets an explicit reset event, followed in the same stream by the retained events, so the client fetches nothing.

**Rule for session events: one stream per browser, owned by a `SharedWorker`.** The worker holds the single `EventSource` and fans change signals out to every tab, so every tab, visible or hidden, stays live: the attention count in a background tab's title and favicon changes as it happens. Each tab tells the worker which run, if any, its session view is showing. The worker multiplexes those run subscriptions onto its one stream by reopening it with the updated subscription set, the event `id` carrying every cursor, so a session view is live the moment it opens with nothing missed; it drops a run subscription when no visible tab shows that run. Over HTTP/1.1 a browser allows six connections per origin across all tabs, and a cleartext Sortie listener always reaches browsers as HTTP/1.1; with one stream per browser the connection count no longer grows with tabs. Where `SharedWorker` is unavailable (it ships in Chrome, Firefox, Safari 16 and later, and recent Chrome for Android), each tab holds one stream with the same multiplexing, and hidden tabs keep their change signals.

| Option for session events | Outcome |
|---|---|
| One stream per browser in a `SharedWorker`, session subscriptions multiplexed (chosen) | Every tab live, hidden or visible; one connection however many tabs are open. Costs a worker lifecycle to manage (tabs attach and detach, the worker outlives any one tab, a stale worker can survive an upgrade until every tab closes) and harder debugging, since the stream lives outside any page's developer tools. |
| One stream per tab with the same multiplexing (fallback) | Ordinary tab behavior with no worker, but each tab holds a connection, so a sixth dashboard tab on one origin stalls every request. Used only where `SharedWorker` is unavailable. |
| A second stream opened by the session view | Three tabs on session views exhaust the browser's budget and every request on the origin stalls. |
| WebSocket | Not in the standard library, needs reverse-proxy configuration for `Upgrade`, and has no browser same-origin protection without an explicit `Origin` check. Becomes the choice, through `github.com/coder/websocket` with approval, only for sustained client-to-server traffic such as co-editing one document or typing into a live session. |

### Gates

| Gate | Needs Node | Runs in |
|---|---|---|
| Type contract test | No | `make test`, every OS |
| Embedded output test: every asset `index.html` references exists | No | `make test`, every OS |
| Lint, type check, format check, output drift | Yes | `make ui-check`, part of `make check`; one Linux CI job on every pull request |

The dashboard CI job is not a required check for now. `make build`, `make test`, and `make lint` never call Node, so the Go-only contributor's bar is unchanged.

### Supply chain

`web/.npmrc` sets `ignore-scripts=true`, so no dependency runs install scripts; `min-release-age=3`, so no version published less than three days ago is installed; and `allow-git=none`, so no dependency resolves from a git reference. npm older than 11.10 skips the last two settings with only a warning, so the Node version CI installs for the dashboard is 24.15.0 or later (bundled npm 11.12.1). Dependencies are pinned to exact versions, `package-lock.json` is committed, and CI installs with `npm ci`. Dependabot covers the npm ecosystem in `web/` with grouped updates. Build-time plugins still execute during `vite build`, so a compromised plugin is not stopped by these settings; exact pins and reviewed updates are the remaining defense.

### Structure for later surfaces

Each screen is a module under `web/src/screens/<name>/`, loaded lazily by its route. A screen may import shared components, the generated API types and client, and the stream client; it never imports another screen. A lint rule enforces the boundary. A new surface is a new module and a new route, with its heavy libraries confined to its own chunk.

### Relation to earlier decisions

ADR-0006 states that any addition must be pure Go with no CGo, and ADR-0008 frames the dashboard as a zero-dependency, self-contained HTML surface rendered with `html/template`. Both remain accepted for what they protect: Go code stays pure Go, the binary has no runtime dependency, and the dashboard tier with its JSON API stays the primary observability surface. This decision narrows their reading in one place: the dashboard's browser code is built from npm packages at build time and ships inside the binary as static files, and the server-rendered page is replaced by that application.

### Considered Options in Detail

| Option | Why not chosen |
|---|---|
| Svelte 5 with Vite | The strongest alternative: smaller bundle, first-party graph editor and primitives, and the best official agent tooling. Loses on the missing official router for plain Svelte, ecosystem libraries still written for Svelte 4 (a virtualizer failed under runes until patched), no TypeScript 7 for `.svelte`, and a history of breaking major versions. SvelteKit's static mode adds server machinery the dashboard never uses and had just shipped a breaking major. |
| Vue 3 with Vite | A credible second with Go-project precedent. Loses on graph-editor and chat-rendering depth, a mid-size bundle with no compensating gain, two component API styles in the training data, and no TypeScript 7 for `.vue`. |
| Preact | React's model at a quarter of the bytes, but the ecosystem arrives through a compat alias each new library must survive, and its dependency tree was the largest measured because React peers install anyway. Bytes do not matter on a LAN. |
| Solid | Fastest and smallest, but a major version has been in prerelease for well over a year, the primitives library is pre-1.0, and no maintained graph editor exists. |
| Lit | No mainstream headless primitives, no graph editor, low release cadence. |
| Vanilla TypeScript | Smallest everything, but live updates that preserve focus, scroll, and selection need keyed reconciliation and a reactive store, which is a framework every agent session would reinvent, and manual escaping makes every new sink a cross-site scripting risk for server-provided text. |
| htmx with Go templates | The only option with no Node at all and a perfect fit for Go owning the meaning of Sortie state, but it moves rendering back into Go against a JSON-API client, and the selection, drawer, command palette, and editors are client state it leaves to extra JavaScript. |
| esbuild or Bun as bundler | esbuild has no hot module replacement and Vite no longer uses it; Bun is a second, unpinned JavaScript runtime with thin framework plugins. |
| pnpm or Bun as package manager | pnpm blocks dependency install scripts and delays new versions by default, but npm reaches the same with three `.npmrc` lines, and pnpm is one more tool to pin and install; npm lacks only pnpm's check that refuses a version whose publishing trust level dropped. Bun blocks install scripts by default, but it is a second JavaScript runtime beside the Node the build already needs. |
| ESLint with typescript-eslint and Prettier | Roughly a hundred packages against a few native binaries, and the TypeScript 6 API alongside TypeScript 7. |
| Plain CSS with CSS Modules | Keeps one hand-written vocabulary and a smaller tree, but Tailwind's theme layer gives one token definition that both utilities and custom properties read, and it is the styling agents and component sources use most. |
| Radix primitives | Equally capable and runtime-proven in the evaluation; Base UI is its maintainers' successor line and the component source's default, so new owned components start there. |
| Build output generated only in CI, placeholder committed | `go install` and the from-source container ship a binary without a dashboard, the release job needs Node, and a release binary is no longer reproducible from the tag with Go alone. |
| Build tag choosing real or placeholder assets | The same `go install` loss when the default excludes the app, and twice the build configurations to lint and test. |
| Hash routing | Saves only the fallback handler; the fragment never reaches the server, so a future login redirect cannot return the operator to the page asked for. |
| Hand-written TypeScript types, `tygo`, or an OpenAPI framework | Hand-written types are a second copy that structural typing cannot check against the wire. `tygo` cannot see the unexported response types and maps nullable pointers to optional fields, which disagrees with `encoding/json`. OpenAPI frameworks either rewrite every handler or invert the source of truth, and all of them add module dependencies for what a short standard-library test does. |

## Consequences

### Positive

- Operators keep one binary with nothing to install and nothing fetched at runtime, through every install channel including `go install`.
- Go-only contributors, the release job, and the container build stay Go-only, and the Go-side gates (type contract and embedded assets) run on every OS without Node.
- A response-type change in Go cannot drift silently from the client, and third parties get an OpenAPI description derived from the same walk.
- Each later surface lands as one lazily loaded module, using mainstream libraries agents already know.

### Negative

- **Concurrent dashboard pull requests conflict on the committed output.** Content-hashed names cascade, so almost every change rewrites the entry chunk. Resolution is mechanical (take either side, rebuild, commit) and the drift gate rejects a hand merge, but every such conflict costs a rebuild.
- **The drift gate is not a required check.** A pull request whose committed output is stale shows a red dashboard job but can still merge, and the binary then embeds the stale output until the next rebuild.
- **Dependabot npm updates that change the bundle turn the drift gate red** and need a rebuild before merge.
- **React carries the largest runtime of the candidates**, about 115 KiB gzip for React, a router, a dialog, and a virtual list before application code, and React has the slowest renderer of the mainstream frameworks. Neither matters at a LAN dashboard's scale.
- **Hook rules are a recurring bug source** (stale closures, dependency arrays); lint enforcement mitigates it, not removes it.
- **The stream lives in a `SharedWorker`.** Its lifecycle (attach, detach, reconnect, replacement after an upgrade) is code the project owns, it is harder to inspect than a page's own network traffic, and the per-tab fallback path must be kept working beside it.
- **npm's safety controls are opt-in.** They hold only while `web/.npmrc` keeps them and the Node pin stays at 24.15.0 or later, and npm has no check that refuses a version whose publishing trust level dropped.
- **Oxfmt is pre-1.0** and may change formatting between minor versions.
- **The repository gains a JavaScript toolchain** to keep current, with its own release cadence and security advisories.

## Confirmation

1. `go build`, `go vet ./...` and `go test ./...` pass on a clean checkout with no Node installed, and the resulting binary serves the full dashboard at `/ui/`.
2. Adding a field to a response type turns `go test` red until the generated TypeScript and OpenAPI description are regenerated.
3. Committing output that differs from a fresh build, including a missing new file, fails the dashboard CI job.
4. A browser loading a deep link under `/ui/` from the binary renders the app with the content security policy enforced and no console violations.
5. With the server's write timeout in force, a stream stays open past it; shutting the server down with a stream open returns without waiting out the shutdown deadline; and a client that stops reading never delays the orchestrator's event loop.
6. Ten dashboard tabs on one origin, hidden and visible, several of them on session views, all stay live (a hidden tab's title follows the attention count) and keep their requests responsive over plain HTTP, with one stream open for the browser.
7. With the committed `.npmrc` and the pinned Node, `npm ci` installs a dependency that declares a `preinstall` script without running it, and `npm install` refuses a version published less than three days ago.
