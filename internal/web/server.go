// Package web serves the interface.
//
// Pages are rendered on the server and swapped by HTMX. Every filter lives in
// the URL, so a filtered view can be pasted into a message and open the same
// thing for the next person, and the back button works because the same
// handler can render either a fragment or a whole page depending on how it was
// asked.
package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server holds everything the handlers need.
type Server struct {
	Store  *store.Store
	Engine *engine.Engine
	Log    *slog.Logger
	// StaleAfter is how long a collected value may go unconfirmed before the
	// detection pass that follows a run raises it. Zero turns the check off
	// rather than defaulting to a number nobody chose.
	StaleAfter time.Duration

	tmpl *template.Template
	hub  *hub
	mu   sync.Mutex
	// running guards against two collection runs at once. Opening a second
	// wave of sessions into the estate because somebody clicked twice is not
	// a thing a tool should allow.
	running bool
}

// New builds a server and parses templates once.
func New(st *store.Store, eng *engine.Engine, log *slog.Logger) (*Server, error) {
	s := &Server{Store: st, Engine: eng, Log: log, hub: newHub()}
	if eng != nil {
		eng.Wire()
	}
	t, err := template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	return s, nil
}

// Routes returns the handler for the whole application.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /hosts", s.handleHosts)
	mux.HandleFunc("GET /hosts/{id}", s.handleHost)
	mux.HandleFunc("GET /stacks", s.handleStacks)
	mux.HandleFunc("GET /charts", s.handleCharts)
	mux.HandleFunc("GET /discrepancies", s.handleDiscrepancies)
	mux.HandleFunc("POST /discrepancies/{id}/dispose", s.handleDispose)
	mux.HandleFunc("POST /hosts/{id}/address", s.handleSelectAddress)
	mux.HandleFunc("GET /runs", s.handleRuns)
	mux.HandleFunc("POST /collect", s.handleCollect)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	return mux
}

type pageData struct {
	Title    string
	Nav      string
	Content  template.HTML
	OpenDisc int
}

// render writes either the fragment or the whole page.
//
// Getting this wrong is the classic HTMX failure: the browser caches a
// fragment against a URL, and pressing back later paints a bare table where a
// page should be. Varying on the request header keeps the two responses apart
// in any cache between here and the browser.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title, nav string, data any) {
	w.Header().Set("Vary", "HX-Request, HX-History-Restore-Request")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		w.Write(buf.Bytes())
		return
	}

	open := 0
	if _, total, err := s.Store.DiscrepancyCounts(r.Context()); err == nil {
		open = total
	}
	page := pageData{Title: title, Nav: nav, Content: template.HTML(buf.String()), OpenDisc: open}
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", page); err != nil {
		s.fail(w, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("render", "err", err)
	http.Error(w, "something went wrong rendering this page", http.StatusInternalServerError)
}

type homeData struct {
	Coverage  store.Coverage
	Counts    map[model.DiscrepancyKind]int
	Total     int
	Recent    []model.Discrepancy
	LastRun   *store.RunSummary
	StaleDays int
}

// handleHome shows the disagreements first.
//
// The landing page is not a count of hosts. Anyone can count hosts. What a
// written inventory cannot tell you is where it is wrong, so that is what
// opens.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const staleAfter = 14 * 24 * time.Hour

	cov, err := s.Store.Coverage(ctx, staleAfter)
	if err != nil {
		s.fail(w, err)
		return
	}
	counts, total, err := s.Store.DiscrepancyCounts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	recent, err := s.Store.Discrepancies(ctx, store.DiscrepancyFilter{Disposition: model.DispOpen}, 25)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := homeData{Coverage: cov, Counts: counts, Total: total, Recent: recent, StaleDays: 14}
	if runs, err := s.Store.Runs(ctx, 1); err == nil && len(runs) > 0 {
		d.LastRun = &runs[0]
	}
	s.render(w, r, "home.html", "Disagreements", "home", d)
}

type hostsData struct {
	Rows    []store.HostRow
	Attrs   []store.AttrInfo
	Columns []string
	Filter  store.HostFilter
	Query   template.URL
}

// defaultColumns are the attributes shown in the table. They are the ones most
// inventories are kept for; anything else lives on the host page, where high
// cardinality costs nothing.
var defaultColumns = []string{"os.version", "app.name", "role", "env", "site"}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.HostFilter{
		Attr:     q.Get("attr"),
		Value:    q.Get("value"),
		Stack:    q.Get("stack"),
		Search:   q.Get("q"),
		Unknown:  q.Get("unknown"),
		Because:  q.Get("because"),
		Unparsed: q.Get("unparsed"),
		// The UI calls this "disagrees" everywhere else, and a URL is part of
		// the UI: it gets pasted into chat messages and tickets.
		Contested: q.Get("disagrees") == "1" || q.Get("contested") == "1",
	}
	rows, err := s.Store.Hosts(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	attrs, err := s.Store.Attrs(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "hosts.html", "Inventory", "hosts", hostsData{
		Rows: rows, Attrs: attrs, Columns: defaultColumns, Filter: f,
		Query: template.URL(r.URL.RawQuery),
	})
}

type hostData struct {
	CI         model.CI
	Facts      []model.Fact
	Conflicts  map[string][]model.Fact
	Identities []model.Identity
	Addresses  []store.Address
	Profiles   []store.ConnProfile
	Reach      store.Reachability
	Discs      []model.Discrepancy
	Stacks     []string
	// FreeText names the attributes no rule was ever written for, so that the
	// value column shows what the source said instead of calling it unparsed.
	FreeText map[string]bool
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	ci, err := s.Store.CI(ctx, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	facts, err := s.Store.Facts(ctx, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	conflicts := map[string][]model.Fact{}
	for _, f := range facts {
		if !f.Contested {
			continue
		}
		all, err := s.Store.Conflicts(ctx, id, f.Attr, f.Scope)
		if err != nil {
			s.fail(w, err)
			return
		}
		conflicts[f.Attr] = all
	}
	ids, _ := s.Store.Identities(ctx, id)
	addrs, _ := s.Store.Addresses(ctx, id)
	profiles, _ := s.Store.ConnProfiles(ctx, id)
	reach, _ := s.Store.Reachability(ctx, id)
	discs, _ := s.Store.Discrepancies(ctx, store.DiscrepancyFilter{CIID: id}, 50)
	freeText, err := s.Store.FreeTextAttrs(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	s.render(w, r, "host.html", ci.Label, "hosts", hostData{
		CI: ci, Facts: facts, Conflicts: conflicts, Identities: ids,
		Addresses: addrs, Profiles: profiles, Reach: reach, Discs: discs,
		FreeText: freeText,
	})
}

func (s *Server) handleStacks(w http.ResponseWriter, r *http.Request) {
	views, err := s.Store.Stacks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	type stackWithMembers struct {
		View    store.StackView
		Members []store.HostRow
	}
	out := make([]stackWithMembers, 0, len(views))
	for _, v := range views {
		m, err := s.Store.StackMembers(r.Context(), v.Stack.Name)
		if err != nil {
			s.fail(w, err)
			return
		}
		out = append(out, stackWithMembers{View: v, Members: m})
	}
	s.render(w, r, "stacks.html", "Stacks", "stacks", out)
}

func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attrs, err := s.Store.Attrs(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	type chartsData struct {
		Charts  []Chart
		Skipped []store.AttrInfo
	}
	var d chartsData
	requested := r.URL.Query()["attr"]
	for _, a := range attrs {
		if len(requested) > 0 && !contains(requested, a.Attr) {
			continue
		}
		if !a.Chartable && len(requested) == 0 {
			d.Skipped = append(d.Skipped, a)
			continue
		}
		buckets, err := s.Store.Distribution(ctx, a.Attr)
		if err != nil {
			s.fail(w, err)
			return
		}
		title := a.Label
		if title == "" {
			title = a.Attr
		}
		d.Charts = append(d.Charts, NewChart(a.Attr, title, buckets))
	}
	s.render(w, r, "charts.html", "Charts", "charts", d)
}

func (s *Server) handleDiscrepancies(w http.ResponseWriter, r *http.Request) {
	f := store.DiscrepancyFilter{
		Kind:        model.DiscrepancyKind(r.URL.Query().Get("kind")),
		Disposition: model.Disposition(r.URL.Query().Get("disposition")),
	}
	if f.Disposition == "" {
		f.Disposition = model.DispOpen
	}
	list, err := s.Store.Discrepancies(r.Context(), f, 500)
	if err != nil {
		s.fail(w, err)
		return
	}
	counts, total, _ := s.Store.DiscrepancyCounts(r.Context())
	s.render(w, r, "discrepancies.html", "Disagreements", "discrepancies", struct {
		List   []model.Discrepancy
		Counts map[model.DiscrepancyKind]int
		Total  int
		Filter store.DiscrepancyFilter
	}{list, counts, total, f})
}

func (s *Server) handleDispose(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	disp := model.Disposition(r.FormValue("disposition"))
	switch disp {
	case model.DispConfirmMachine, model.DispKeepDocument, model.DispAccepted:
	default:
		http.Error(w, "unknown disposition", http.StatusBadRequest)
		return
	}
	actor := actorOf(r)
	if err := s.Store.Dispose(r.Context(), id, disp, actor, r.FormValue("note")); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("HX-Trigger", "discrepancy-disposed")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<p class="muted">Recorded as %s by %s.</p>`,
		template.HTMLEscapeString(string(disp)), template.HTMLEscapeString(actor))
}

func (s *Server) handleSelectAddress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	addr := r.FormValue("address")
	if err := s.Store.SelectAddress(r.Context(), id, addr, actorOf(r)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/hosts/"+id, http.StatusSeeOther)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.Runs(r.Context(), 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "runs.html", "Collection runs", "runs", runs)
}

// handleCollect starts a run in the background and answers immediately. The
// browser follows along over the event stream rather than holding a request
// open for as long as the estate takes.
func (s *Server) handleCollect(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `<p class="muted">A collection run is already in progress.</p>`)
		return
	}
	s.running = true
	s.mu.Unlock()

	actor := actorOf(r)
	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()

		progress := make(chan engine.Progress, 64)
		go func() {
			for p := range progress {
				s.hub.broadcast(p)
			}
		}()
		sum, err := s.Engine.Collect(ctx, engine.Options{Actor: actor}, progress)
		if err != nil {
			s.Log.Error("collection run", "err", err)
		}
		// Detection follows collection without being asked. The landing page
		// is the list of disagreements; leaving it to a separate command
		// would let the page report none when what it means is that nobody
		// has looked since the facts changed.
		det, derr := s.Engine.Detect(ctx, s.StaleAfter)
		if derr != nil {
			s.Log.Error("detection pass", "err", derr)
		}
		s.hub.broadcast(engine.Progress{
			RunID: sum.ID, Label: "run finished", Done: sum.Targets, Total: sum.Targets,
			Outcome: model.OutcomeOK,
			Detail: fmt.Sprintf("%d ok, %d failed, %d disagreements",
				sum.Succeeded, sum.Failed, det.Conflicts+det.Unparsed+det.Stale+det.Undocumented),
		})
	}()

	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `<p class="muted">Collection started. Progress appears below.</p>`)
}

// handleEvents streams run progress.
//
// One stream carries every host's result as it lands. Polling for this would
// mean either a chatty request every second or a view that lags behind the
// thing it is reporting on.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case p, ok := <-ch:
			if !ok {
				return
			}
			var buf bytes.Buffer
			if err := s.tmpl.ExecuteTemplate(&buf, "progress.html", p); err != nil {
				s.Log.Error("progress fragment", "err", err)
				continue
			}
			// An SSE line ends at CR, LF or CRLF, and part of this fragment
			// is text a remote host chose. Both are flattened so the frame
			// cannot be cut short and the remainder read as SSE fields.
			flat := strings.NewReplacer("\r", " ", "\n", " ").Replace(buf.String())
			fmt.Fprintf(w, "event: progress\ndata: %s\n\n", flat)
			flusher.Flush()
		}
	}
}

// actorOf names whoever is acting. Authentication is deliberately not built
// in: this runs behind something that already does it, and a login page
// written here would be a second, weaker one.
func actorOf(r *http.Request) string {
	for _, h := range []string{"X-Forwarded-User", "X-Remote-User", "X-Auth-Request-Preferred-Username"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return "anonymous"
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// hub fans run progress out to connected browsers.
type hub struct {
	mu   sync.Mutex
	subs map[chan engine.Progress]struct{}
}

func newHub() *hub { return &hub{subs: map[chan engine.Progress]struct{}{}} }

func (h *hub) subscribe() chan engine.Progress {
	ch := make(chan engine.Progress, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan engine.Progress) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// broadcast never blocks. A browser that cannot keep up misses updates; it
// must not be able to stall a collection run.
func (h *hub) broadcast(p engine.Progress) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}
