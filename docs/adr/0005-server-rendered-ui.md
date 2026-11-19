# 5. Server-rendered HTML, HTMX, and charts drawn in Go

Status: accepted, partly superseded

> Since November 2026, the value from a host always wins, and kryostate has no
> disagreements, findings or decisions. Read the [user guide](../user-guide.md) for the current behaviour.

## Context

The interface must list hosts with filters, show the values of one host and
their sources, and draw the distribution of values. At the time of this
decision, it also let an operator record a decision about a disagreement.
Each of these is a request, a query and some HTML.

A JavaScript application with a JSON API adds a build step, a dependency tree
and a second data model. Two data models can disagree. kryostate exists
because copies of the same data drift apart.

## Decision

kryostate uses Go `html/template` on the server, HTMX for partial updates, and
SVG charts that Go makes.

**The URL is the only source of filter state.** Each filter is a query
parameter. Two people who open the same URL see the same view.

**One handler serves a page and a fragment.** If the request has
`HX-Request: true`, the handler returns the content template. If not, it puts
the content in the layout. kryostate treats `HX-History-Restore-Request` as a
page request. Each response sets `Vary: HX-Request, HX-History-Restore-Request`,
so that a cache does not return a fragment as a page.

**Go makes the charts as SVG.** Each mark is a link to the filtered table
that it counts. A chart library would add a build step, a supply chain and a
reload bug on each swap. Unknown buckets have an SVG hatch pattern, so you can
see them without colour.

**Long tails fold, but unknown buckets never fold.** A chart card shows 8
bars, and the overlay shows 30. kryostate folds the remaining values into
"Other", which is not a link. Unknown buckets are never folded, because they
show the hosts that need work.

**All assets are in the binary.** HTMX and its SSE extension are in
`internal/web/static`, and Go embeds them. A host with no route to the
internet gets a working interface.

**Glass effects are only on the chrome.** The header, navigation and cards
can use them. Surfaces with tables are solid and have high contrast, because a
blurred background makes version numbers difficult to read.

## Consequences

- There is no build step. `go build` makes the application.
- At the time of this decision, there was no JSON API. A read-only JSON API
  under `/api/v1` now exists as a second representation. The interface does
  not use it.
- Handler tests examine the real HTML, the fragment and page behaviour, and
  the `Vary` header.
- The server renders each page, so a page with many rows uses server time. At
  this size, the time is microseconds.
- Interactions that need client state, such as drag-to-reorder, need
  JavaScript.

## Alternatives considered

| Alternative | Reason for rejection |
|---|---|
| A single-page application with a JSON API | A second data model and a build step, for a tool that exists because copies of data drift apart. |
| Plain forms with full page loads | This works. But HTMX is only 50 KB in the binary (14 KB compressed), and it keeps the scroll position. |
| A chart library | The charts are bars with labels and counts. Refer to the Decision section. |
