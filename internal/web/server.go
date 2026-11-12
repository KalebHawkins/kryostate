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
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
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
	// overview counts it as stale. Zero keeps the default window.
	StaleAfter time.Duration
	// TrustedOrigins are extra origins allowed to submit forms, for a proxy
	// that serves this interface under a different host name than it
	// forwards. Same-origin requests need no entry.
	TrustedOrigins []string
	// Config says where custom collector files are looked for, for the
	// collectors page.
	Config ConfigFiles
	// AllowRevealing lets the collectors page switch on catalog entries that
	// read the contents of files or variables. Off by default: those values
	// may be secrets, and the page has no login of its own.
	AllowRevealing bool
	// Scheduler runs collectors on their schedules. The schedules page reads
	// it whether or not it was started, and a manual run passes its gate.
	Scheduler *engine.Scheduler

	tmpl *template.Template
	// guide is the rendered user guide. It is fixed at build time, so it is
	// rendered once rather than on every request.
	guide    template.HTML
	guideTOC []tocEntry

	collectorGuide    template.HTML
	collectorGuideTOC []tocEntry
	queryGuide        template.HTML
	queryGuideTOC     []tocEntry
	hub               *hub
	mu                sync.Mutex
	// running guards against two collection runs at once. Opening a second
	// wave of sessions into the estate because somebody clicked twice is not
	// a thing a tool should allow.
	running bool
	// titles names attributes after the collector choices that record them,
	// refreshed for each page so a newly switched-on collector is named at once.
	titles atomic.Pointer[map[string]string]
}

// New builds a server and parses templates once.
func New(st *store.Store, eng *engine.Engine, log *slog.Logger) (*Server, error) {
	s := &Server{Store: st, Engine: eng, Log: log, hub: newHub()}
	if eng != nil {
		eng.Wire()
		s.Scheduler = engine.NewScheduler(eng, nil)
		s.Scheduler.OnProgress = s.hub.broadcast
	}
	t, err := template.New("").Funcs(s.funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	s.renderDocs()
	return s, nil
}

// Routes returns the handler for the whole application.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /hosts", s.handleHosts)
	mux.HandleFunc("GET /hosts.csv", s.handleHostsCSV)
	mux.HandleFunc("GET /hosts.json", s.handleHostsJSON)
	mux.HandleFunc("GET /hosts/{id}", s.handleHost)
	mux.HandleFunc("GET /stacks", s.handleStacks)
	mux.HandleFunc("GET /stacks/{name}", s.handleStack)
	mux.HandleFunc("GET /stacks/{name}/hosts", s.handleStackHosts)
	mux.HandleFunc("GET /stacks/{name}/query", s.handleStackQueryPreview)
	mux.HandleFunc("GET /charts", s.handleCharts)
	mux.HandleFunc("POST /hosts/{id}/address", s.handleSelectAddress)
	mux.HandleFunc("POST /hosts/{id}/application", s.handleApplication)
	mux.HandleFunc("POST /hosts", s.handleCreateHost)
	mux.HandleFunc("POST /hosts/{id}/retire", s.handleRetire)
	mux.HandleFunc("POST /hosts/{id}/unretire", s.handleUnretire)
	mux.HandleFunc("POST /hosts/{id}/connection", s.handleConnection)
	mux.HandleFunc("POST /hosts/{id}/connection/delete", s.handleDeleteConnection)
	mux.HandleFunc("POST /hosts/{id}/checks", s.handleAddCheck)
	mux.HandleFunc("POST /hosts/{id}/checks/delete", s.handleDeleteCheck)
	mux.HandleFunc("POST /stacks", s.handleCreateStack)
	mux.HandleFunc("POST /stacks/edit", s.handleEditStack)
	mux.HandleFunc("POST /stacks/delete", s.handleDeleteStack)
	mux.HandleFunc("POST /stacks/members", s.handleAddMember)
	mux.HandleFunc("POST /stacks/members/remove", s.handleRemoveMember)
	mux.HandleFunc("GET /collectors", s.handleCollectors)
	mux.HandleFunc("GET /collectors/{id}", s.handleCollector)
	mux.HandleFunc("POST /collectors/choose", s.handleChoose)
	mux.HandleFunc("POST /collectors/remove", s.handleUnchoose)
	mux.HandleFunc("POST /collectors/try", s.handleTry)
	mux.HandleFunc("POST /collectors/where", s.handleChooseWhere)
	mux.HandleFunc("GET /collectors/target/preview", s.handleTargetPreview)
	mux.HandleFunc("GET /collectors/target/hosts", s.handleTargetHosts)
	mux.HandleFunc("GET /collectors/try/hosts", s.handleTryHosts)
	mux.HandleFunc("POST /stacks/hosts", s.handleBulkHosts)
	mux.HandleFunc("POST /stacks/query", s.handleStackQuery)
	mux.HandleFunc("POST /collectors/group", s.handleChooseGroup)
	mux.HandleFunc("GET /runs", s.handleRuns)
	mux.HandleFunc("GET /schedules", s.handleSchedules)
	mux.HandleFunc("POST /schedules", s.handleSetSchedule)
	mux.HandleFunc("POST /schedules/spread", s.handleSpreadSchedules)
	mux.HandleFunc("GET /changes", s.handleChanges)
	mux.HandleFunc("GET /guide", s.handleGuide)
	mux.HandleFunc("GET /guide/collectors", s.handleCollectorGuide)
	mux.HandleFunc("GET /guide/queries", s.handleQueryGuide)
	mux.HandleFunc("POST /collect", s.handleCollect)
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	// Every POST here changes the inventory, and the interface has no login
	// of its own. Without this, any web page the operator visits could submit
	// those forms to a kryostate running on localhost. The check uses the
	// Sec-Fetch-Site and Origin headers browsers send, so it needs no token
	// in the forms and leaves non-browser clients alone.
	cop := http.NewCrossOriginProtection()
	for _, o := range s.TrustedOrigins {
		if err := cop.AddTrustedOrigin(o); err != nil && s.Log != nil {
			s.Log.Error("trusted origin", "origin", o, "err", err)
		}
	}
	return cop.Handler(mux)
}

type pageData struct {
	Title   string
	Nav     string
	Content template.HTML
	Flash   *flash
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

	if s.Store != nil {
		m := s.attrTitles(r.Context())
		s.titles.Store(&m)
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true" {
		w.Write(buf.Bytes())
		return
	}

	page := pageData{Title: title, Nav: nav, Content: template.HTML(buf.String()), Flash: takeFlash(w, r)}
	if err := s.tmpl.ExecuteTemplate(w, "layout.html", page); err != nil {
		s.fail(w, err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("render", "err", err)
	http.Error(w, "something went wrong rendering this page", http.StatusInternalServerError)
}

// defaultColumns are the attributes shown in the table. They are the ones most
// inventories are kept for; anything else lives on the host page, where high
// cardinality costs nothing.
var defaultColumns = []string{"os.version", model.AttrApplication}

// columnsFrom reads the chosen columns from the URL. Both repeated
// "col=a&col=b" (what a form of checkboxes sends) and "col=a,b" (what a person
// types) are accepted.
func columnsFrom(q url.Values) []string {
	var cols []string
	for _, v := range q["col"] {
		for _, c := range strings.Split(v, ",") {
			if c = strings.TrimSpace(c); c != "" && !contains(cols, c) {
				cols = append(cols, c)
			}
		}
	}
	if len(cols) == 0 {
		return defaultColumns
	}
	return cols
}

// csvSafe defuses a value that a spreadsheet would run as a formula. Values
// here come from documents and from remote hosts, neither of which should be
// able to put a live formula in front of whoever opens the export.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

type hostData struct {
	CI         model.CI
	Facts      []model.Fact
	Identities []model.Identity
	Addresses  []store.Address
	Profiles   []store.ConnProfile
	Reach      store.Reachability
	Stacks     []string
	// Memberships are the stacks this host is in, with its role in each.
	Memberships []store.Membership
	// AllStacks feeds the "add to stack" suggestions.
	AllStacks []string
	Checks    []store.Check
	// SSH is the ssh profile when the browser may edit it, and SSHLocked
	// says one exists that only the command line may change.
	SSH       *store.ConnProfile
	SSHLocked bool
	// FreeText names the attributes no rule was ever written for, so that the
	// value column shows what the source said instead of calling it unparsed.
	FreeText map[string]bool
	// Testable lists the collectors the Test form on this page offers.
	Testable []testOption
	// Labels name the attributes recorded by switched-on catalog templates,
	// such as "openssl package version" for pkg.openssl.version.
	Labels map[string]string
	// Changed holds the recent change behind a fact, by assertion id, for the
	// "changed" badge.
	Changed map[int64]*store.Change
	// Collectors are what runs on this host and why, and what is on but
	// skips it.
	Collectors hostCollectors
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
	ids, _ := s.Store.Identities(ctx, id)
	addrs, _ := s.Store.Addresses(ctx, id)
	profiles, _ := s.Store.ConnProfiles(ctx, id)
	reach, _ := s.Store.Reachability(ctx, id)
	freeText, err := s.Store.FreeTextAttrs(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}

	d := hostData{
		CI: ci, Facts: facts, Identities: ids,
		Addresses: addrs, Profiles: profiles, Reach: reach,
		FreeText: freeText,
	}
	if d.Memberships, err = s.Store.MembershipsOf(ctx, id); err != nil {
		s.fail(w, err)
		return
	}
	if d.Checks, err = s.Store.Checks(ctx, id); err != nil {
		s.fail(w, err)
		return
	}
	if views, err := s.Store.Stacks(ctx); err == nil {
		for _, v := range views {
			d.AllStacks = append(d.AllStacks, v.Stack.Name)
		}
	}
	for i, p := range profiles {
		if p.Transport != "ssh" {
			continue
		}
		if webEditable(p) {
			d.SSH = &profiles[i]
		} else {
			d.SSHLocked = true
		}
	}
	d.Labels = s.attrTitles(ctx)
	d.Changed = s.hostChanges(ctx, id)
	d.Collectors = s.hostCollectorsFor(ctx, id, facts, d.Changed)
	if choices, err := s.Store.Choices(ctx); err == nil {
		for _, c := range choices {
			if !c.InCatalog() {
				continue
			}
			d.Testable = append(d.Testable, testOption{Value: "choice:" + strconv.FormatInt(c.ID, 10), Label: c.Label(), Group: "Switched on"})
		}
	}
	if s.Engine != nil && s.Engine.Specs != nil {
		seen := map[string]bool{}
		for _, sp := range s.Engine.Specs.Specs {
			if !seen[sp.ID] {
				seen[sp.ID] = true
				d.Testable = append(d.Testable, testOption{Value: "spec:" + sp.ID, Label: sp.ID + " (" + sp.Attr + ")", Group: "Always collected and your own"})
			}
		}
	}
	s.render(w, r, "host.html", ci.Label, "hosts", d)
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

// handleApplication records the application a person says a host runs. It is
// the only attribute the page accepts: role and stack are set as stack
// membership, and everything else is what the host reports. No reason is
// asked for: the audit log records who set it, when, and that it was set
// here.
func (s *Server) handleApplication(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.CI(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}
	value := strings.TrimSpace(r.FormValue("application"))
	if value == "" {
		refuse(w, r, hostURL(id), errors.New("name the application"))
		return
	}
	if err := s.Store.SetApplication(r.Context(), id, value, actorOf(r), "set on the host page"); err != nil {
		refuse(w, r, hostURL(id), err)
		return
	}
	done(w, r, hostURL(id), "Recorded the application as "+value+".")
}

func (s *Server) handleGuide(w http.ResponseWriter, r *http.Request) {
	s.renderGuide(w, r, "/guide", s.guide, s.guideTOC)
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.Store.Runs(r.Context(), 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	s.render(w, r, "runs.html", "Collection runs", "runs", struct {
		Runs    []store.RunSummary
		Running bool
		// Elsewhere is a collection another process is running.
		Elsewhere string
	}{runs, running, s.otherCollection(r.Context())})
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
	// Another process on the same database (a kryostate refresh in a shell)
	// may already be collecting; this does not start a second wave.
	releaseLease, other, err := s.takeLease(r.Context(), actor)
	if err != nil || other != "" {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
		if err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, noticeHTML(other))
		return
	}
	go func() {
		defer releaseLease()
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		// A manual run and the scheduler must not become two waves into the
		// same estate: this waits for scheduled sessions already open, and
		// the scheduler opens none while it runs.
		if s.Scheduler != nil {
			gate := s.Scheduler.Gate()
			if !gate.BeginManual() {
				return
			}
			defer gate.EndManual()
		}

		progress := make(chan engine.Progress, 64)
		go func() {
			for p := range progress {
				s.hub.broadcast(p)
			}
		}()
		sum, err := s.Engine.Collect(ctx, engine.Options{Actor: actor, Trigger: "web"}, progress)
		if err != nil {
			s.Log.Error("collection run", "err", err)
		}
		s.hub.broadcast(engine.Progress{
			RunID: sum.ID, Label: "run finished", Done: sum.Targets, Total: sum.Targets,
			Outcome: model.OutcomeOK,
			Detail:  fmt.Sprintf("%d ok, %d failed", sum.Succeeded, sum.Failed),
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
