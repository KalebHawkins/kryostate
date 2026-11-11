# 5. Server-rendered HTML, HTMX, and charts drawn in Go

Status: accepted

## Context

The interface has to do four things: list hosts with filters, show one host's
facts and their provenance, draw distributions of values, and let an operator
record a decision about a disagreement. All four are a request, a query, and
some HTML.

The default answer in 2025 is a JavaScript application with a JSON API behind
it. That buys client-side routing and optimistic updates, and costs a build
step, a dependency tree, a second data model, and a second set of bugs where
the two disagree. For a tool whose entire thesis is that duplicated state
drifts, maintaining a client-side copy of the inventory would be funny.

## Decision

Go's `html/template` on the server, HTMX for partial updates, charts rendered
as SVG by Go.

**The URL is the only source of truth for filter state.** Every filter —
attribute, value, stack, search, contested, the three kinds of unknown — is a
query parameter. A filtered view can be pasted into a message and opens the
same thing for the next person. There is no client-side state to get out of
step with it.

**One handler serves both a page and a fragment.** With `HX-Request: true` it
returns the content template; otherwise it wraps it in the layout. Every
response sets `Vary: HX-Request`, because without it a cache between here and
the browser will serve a fragment as a page, and the back button paints a bare
table where a page should be. `HX-History-Restore-Request` is treated as a page
request for the same reason.

**Charts are SVG strings built in Go.** Each bar is an `<a>` with `hx-get`,
`hx-target` and `hx-push-url`, so clicking a bar lands on the filtered table it
counted. A chart library would add a build step, a supply chain, and a
re-initialisation bug on every swap, in exchange for an animation nobody asked
for. Unknown buckets are drawn with an SVG hatch pattern, so they are
distinguishable from measured values without relying on colour.

**Long tails fold; unknowns never do.** Above fourteen bars the remainder is
collapsed into "N more values", which links nowhere, because it is not a filter
anyone can act on. Unknown buckets are exempt: they are the hosts worth
chasing.

**Every asset ships in the binary.** HTMX and the SSE extension are vendored
into `internal/web/static` and embedded. An estate with no route to the
internet gets a working interface, and nothing here depends on a CDN being up
or honest.

**Glass is applied to chrome only.** Header, navigation, cards. Surfaces
carrying tabular data are solid and high contrast. A translucent, blurred
backdrop behind a column of version numbers is harder to read than a plain one,
and this is a tool for reading version numbers.

## Consequences

- No build step. `go build` produces the application.
- No JSON API. One will be needed eventually and will be added as a second
  representation, not as the thing the interface runs on.
- Handler tests assert real HTML, including the fragment-versus-page behaviour
  and the `Vary` header.
- Rendering happens on the server, so a page with a thousand rows costs server
  time. At this size that is microseconds.
- Interactions needing genuine client state — a drag-reorder, a live-filtering
  text box with no round trip — are not available without writing JavaScript.
  None of the four things this interface does need them.

## Alternatives considered

**A single-page application with a JSON API.** A second data model and a build
step to maintain, for a tool whose point is that two copies of the truth drift
apart.

**Plain forms with full page loads.** Would work. HTMX costs 50 KB in the
binary, 14 KB gzipped on the wire, and keeps scroll position when disposing of
a disagreement, which matters when working through a list of them.

**A charting library.** See above. The charts are bars with labels and counts.
