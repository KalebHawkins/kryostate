package web

import (
	"context"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/docs"
	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/model"
)

// ConfigFiles says where this process looks for the files a person writes,
// and which of them it found. The collectors page shows it, because "where do
// I put my file" is the first question anyone adding a collector asks.
type ConfigFiles struct {
	Dir        string
	Collectors string // path loaded, or blank
	Rules      string
	Externals  string
	// CollectorsErr is why the collectors file could not be loaded when the
	// server started, in which case none of its collectors run.
	CollectorsErr string
}

// Where a collector comes from. These are the values of the source filter.
const (
	srcBuiltin  = "builtin"
	srcCatalog  = "catalog"
	srcOwn      = "own"
	srcExternal = "external"
)

var sourceLabels = map[string]string{
	srcBuiltin:  "built-in",
	srcCatalog:  "catalog",
	srcOwn:      "your own",
	srcExternal: "external",
}

// collectorCategory is one heading of the list and one value of the category
// filter. Catalog entries name theirs; built-in collectors read the operating
// system and hardware, and anything a person wrote is custom.
type collectorCategory struct{ Key, Title string }

var collectorCategories = []collectorCategory{
	{"os", "OS & hardware"},
	{"runtimes", "Runtimes & packages"},
	{"web", "Web servers"},
	{"files", "Files & config"},
	{"custom", "Custom"},
}

func categoryKey(title string) string {
	for _, c := range collectorCategories {
		if c.Title == title {
			return c.Key
		}
	}
	return slug(title, map[string]int{})
}

// collectorRow is one collector in the list, whatever its source.
type collectorRow struct {
	ID          string // the part of the URL after /collectors/
	Title       string
	Description string
	Category    string
	Source      string
	// On is true for anything that runs: a switched-on catalog entry, and
	// every built-in, hand-written and external collector.
	On      bool
	Status  string
	Where   string
	Systems []string
	Attrs   []string
	// AttrExample marks Attrs as an example of what a template would record,
	// because nothing has been switched on from it yet.
	AttrExample bool
	Reveals     bool
	Hidden      bool
}

func (r collectorRow) SourceLabel() string { return sourceLabels[r.Source] }

func (r collectorRow) CategoryKey() string { return categoryKey(r.Category) }

// Search is the text the search box matches, lowercased.
func (r collectorRow) Search() string {
	return strings.ToLower(strings.Join(append([]string{r.Title, r.ID, r.Description, r.Category,
		r.SourceLabel(), strings.Join(r.Systems, " ")}, r.Attrs...), " "))
}

// collectorFilter is what the filter bar asks for. It lives in the URL so a
// filtered list can be linked to; the page's script applies the same rules as
// the person types.
type collectorFilter struct{ Q, Cat, Source, Status string }

func (f collectorFilter) match(r collectorRow) bool {
	if f.Cat != "" && r.CategoryKey() != f.Cat {
		return false
	}
	if f.Source != "" && r.Source != f.Source {
		return false
	}
	if (f.Status == "on" && !r.On) || (f.Status == "off" && r.On) {
		return false
	}
	text := r.Search()
	for _, w := range strings.Fields(strings.ToLower(f.Q)) {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

type collectorSection struct {
	collectorCategory
	Rows   []collectorRow
	Hidden bool
}

type groupView struct {
	G         collect.Group
	On, Total int
}

type collectorsData struct {
	Config      ConfigFiles
	FileProblem string
	// WantPath is where a new collectors file goes.
	WantPath   string
	Filter     collectorFilter
	Categories []collectorCategory
	Sources    []collectorCategory
	Sections   []collectorSection
	Shown      int
	Total      int
	Groups     []groupView
	Chips      targetChips
	Own        int
	// Gone are switched-on collectors whose catalog entry this build no
	// longer has. They never run; each is listed so someone can turn it off.
	Gone []collect.Choice
}

// collectorRows lists every collector from every source, in the order the
// page shows them: by category, catalog entries before the built-in ones.
func (s *Server) collectorRows(ctx context.Context) ([]collectorRow, error) {
	choices, err := s.Store.Choices(ctx)
	if err != nil {
		return nil, err
	}
	var rows []collectorRow
	for _, t := range collect.Catalog {
		row := collectorRow{ID: t.ID, Title: t.Title, Description: t.Description, Category: t.Category,
			Source: srcCatalog, Systems: systemNames(t.Systems()), Reveals: t.Reveals, Status: "off", Where: "—"}
		var on []collect.Choice
		for _, c := range choices {
			if c.Template == t.ID {
				on = append(on, c)
				row.Attrs = append(row.Attrs, c.Attr)
			}
		}
		if len(on) > 0 {
			row.On, row.Status = true, "on"
			row.Where = whereText(on[0].Target)
			if len(on) > 1 {
				row.Where = plural(len(on), "value")
			}
		}
		if len(row.Attrs) == 0 {
			row.Attrs = []string{collect.DefaultAttr(t, t.ExampleParam())}
			row.AttrExample = !t.Fixed()
		}
		rows = append(rows, row)
	}

	order, byAttr, err := collect.BuiltinRefs()
	if err != nil {
		return nil, err
	}
	for _, attr := range order {
		specs := byAttr[attr]
		row := collectorRow{ID: collect.RefBuiltin + attr, Title: attrLabel(attr), Category: "OS & hardware",
			Source: srcBuiltin, On: true, Status: "always", Where: "All hosts", Attrs: []string{attr},
			Systems: systemNames(specSystems(specs))}
		if title, desc := collect.BuiltinTitle(attr); title != "" {
			row.Title, row.Description = title, desc
		}
		for _, sp := range specs {
			if row.Description == "" && sp.Comment != "" {
				row.Description = sp.Comment
			}
		}
		rows = append(rows, row)
	}

	for _, sp := range s.Engine.OwnSpecs() {
		rows = append(rows, collectorRow{ID: collect.RefSpec + sp.ID, Title: sp.ID, Description: sp.Comment,
			Category: "Custom", Source: srcOwn, On: true, Status: "on", Where: whereText(engine.SpecTarget(sp)),
			Attrs: []string{sp.Attr}, Systems: systemNames(sp.OS)})
	}
	if s.Engine != nil {
		for _, ex := range s.Engine.Externals {
			rows = append(rows, collectorRow{ID: collect.RefExternal + ex.Name, Title: ex.Name,
				Description: ex.Comment, Category: "Custom", Source: srcExternal, On: true, Status: "on",
				Where: "All hosts", Systems: []string{"KryoState server"}})
		}
	}
	return rows, nil
}

// handleCollectors lists every collector, from every source, with a filter.
// Commands are never taken here: a collector is a command run on every host,
// and this interface has no authentication of its own.
func (s *Server) handleCollectors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := collectorsData{Config: s.Config, WantPath: s.Config.Collectors, FileProblem: s.collectorsFileProblem(),
		Categories: collectorCategories,
		Filter: collectorFilter{Q: r.URL.Query().Get("q"), Cat: r.URL.Query().Get("cat"),
			Source: r.URL.Query().Get("source"), Status: r.URL.Query().Get("status")}}
	for _, k := range []string{srcBuiltin, srcCatalog, srcOwn, srcExternal} {
		d.Sources = append(d.Sources, collectorCategory{k, sourceLabels[k]})
	}
	if d.WantPath == "" && s.Config.Dir != "" {
		d.WantPath = filepath.Join(s.Config.Dir, "collectors.json")
	}
	rows, err := s.collectorRows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, c := range collectorCategories {
		sec := collectorSection{collectorCategory: c, Hidden: true}
		for _, row := range rows {
			if row.CategoryKey() != c.Key {
				continue
			}
			row.Hidden = !d.Filter.match(row)
			if !row.Hidden {
				sec.Hidden = false
				d.Shown++
			}
			if row.Source == srcOwn {
				d.Own++
			}
			d.Total++
			sec.Rows = append(sec.Rows, row)
		}
		if len(sec.Rows) > 0 {
			d.Sections = append(d.Sections, sec)
		}
	}

	choices, err := s.Store.Choices(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, c := range choices {
		if !c.InCatalog() {
			d.Gone = append(d.Gone, c)
		}
	}
	for _, g := range collect.Groups {
		v := groupView{G: g, Total: len(g.Entries)}
		for _, id := range g.Entries {
			if slices.ContainsFunc(choices, func(c collect.Choice) bool { return c.Template == id }) {
				v.On++
			}
		}
		d.Groups = append(d.Groups, v)
	}
	stacks, _, err := s.stackNames(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Chips = chipsFor(tg, stacks)
	s.render(w, r, "collectors.html", "Collectors", "collectors", d)
}

// variantView is one command of a collector: the systems it runs on, the
// command, and what is kept from its output.
type variantView struct {
	Systems []string
	Command string
	Parse   string
	SpecID  string
}

// choiceView is one switched-on use of a catalog entry.
type choiceView struct {
	C      collect.Choice
	Target targetView
}

type collectorDetail struct {
	Row      collectorRow
	T        *collect.Template
	Variants []variantView
	Type     model.ValueType
	Example  string
	// ExampleParam is the value a template's example and definition use.
	ExampleParam string
	Optional     bool
	Scope        string
	Choices      []choiceView
	Allowed      bool
	// Chips are the target editor's quick inserts.
	Chips targetChips
	// Target is where a collector from a file or built in runs.
	Target targetView
	// SpecWhere and SpecStacks are the where and stacks fields of a spec
	// from collectors.json, as written.
	SpecWhere  string
	SpecStacks []string
	// CanTest is true when some host has a connection to test on.
	CanTest bool
	// TrySpecs are the spec ids the Test button runs, for collectors that
	// are not catalog choices.
	TrySpecs []string
	File     string
	External *collect.External
	DefFile  string
	Def      string
	DefNote  string
	DefErr   string
	// ShowCmd is the command line that prints the same definition.
	ShowCmd string
	// Schedules are this collector's rows from the schedules page.
	Schedules   []scheduleRow
	Scheduling  bool
	FileProblem string
}

// handleCollector shows one collector: what it records, the command on each
// system, where it runs, and its definition written out as a person would
// write it in collectors.json.
func (s *Server) handleCollector(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	rows, err := s.collectorRows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	i := slices.IndexFunc(rows, func(row collectorRow) bool { return row.ID == id })
	if i < 0 {
		http.Error(w, "no collector named "+id, http.StatusNotFound)
		return
	}
	d := collectorDetail{Row: rows[i], Allowed: true, ShowCmd: "kryostate collectors -show " + id}
	stacks, members, err := s.stackNames(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	tg, err := s.targetsNow(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Chips = chipsFor(tg, stacks)
	if profiles, err := s.Store.CollectTargets(ctx, ""); err == nil {
		d.CanTest = len(profiles) > 0
	}
	if all, err := s.scheduleRows(ctx); err == nil {
		d.Schedules = schedulesFor(all, id, d.Row.Source)
	}
	d.Scheduling = s.Scheduler != nil && s.Scheduler.Running()

	switch d.Row.Source {
	case srcCatalog:
		t, _ := collect.Lookup(id)
		d.T = &t
		d.Type, d.Example, d.Optional = t.Type, t.Example(), true
		d.Allowed = !t.Reveals || s.AllowRevealing
		if !t.Fixed() {
			d.ExampleParam = collect.ParamText(t, t.ExampleParam())
		}
		for _, v := range t.Variants {
			d.Variants = append(d.Variants, variantView{Systems: systemNames(v.OS), Command: v.Command,
				Parse: collect.DescribeParse(v.Parse)})
		}
		choices, err := s.Store.Choices(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, c := range choices {
			if c.Template != t.ID {
				continue
			}
			d.Choices = append(d.Choices, choiceView{C: c, Target: describeTarget(tg, c.Target, t.Systems(), members)})
		}
		if !t.Fixed() {
			d.ShowCmd += " -param '" + t.ExampleParam() + "'"
			d.DefNote = "Written out for the example value " + d.ExampleParam + ". "
		}
		d.DefNote += "The ids are the entry's own, without the catalog. prefix, so a copy does not clash with the catalog collector if both are on."

	case srcBuiltin:
		_, byAttr, err := collect.BuiltinRefs()
		if err != nil {
			s.fail(w, err)
			return
		}
		specs := byAttr[strings.TrimPrefix(id, collect.RefBuiltin)]
		d.specDetail(specs)
		d.Target = describeTarget(tg, "", specSystems(specs), members)
		d.Example = collect.AttrExample(specs[0].Attr)
		d.DefNote = "These specs are built into KryoState. To write one like them, copy a spec into your own collectors.json and give it an id of its own: ids must be unique, and the built-in ones are taken."

	case srcOwn:
		for _, sp := range s.Engine.OwnSpecs() {
			if collect.RefSpec+sp.ID == id {
				d.specDetail([]collect.Spec{sp})
				d.SpecWhere, d.SpecStacks = sp.Where, sp.Stacks
				d.Target = describeTarget(tg, engine.SpecTarget(sp), sp.OS, members)
			}
		}
		d.File = s.Config.Collectors
		d.FileProblem = s.collectorsFileProblem()
		d.DefNote = "As loaded from your collectors.json when the server started. Edit that file on the server to change it."

	case srcExternal:
		ex, _ := s.Engine.External(strings.TrimPrefix(id, collect.RefExternal))
		ex = engine.RedactEnv(ex)
		d.External = &ex
		d.File = s.Config.Externals
		d.DefNote = "As loaded from externals.json when the server started. Values of environment variables are hidden here, because they may be the program's own credential."
	}

	file, def, err := s.Engine.Definition(id, "")
	if err != nil {
		d.DefErr = err.Error()
	}
	d.DefFile, d.Def = file, string(def)
	s.render(w, r, "collector.html", d.Row.Title+" · Collectors", "collectors", d)
}

// specDetail fills in what a collector made of specs shows.
func (d *collectorDetail) specDetail(specs []collect.Spec) {
	if len(specs) == 0 {
		return
	}
	d.Type, d.Optional, d.Scope = specs[0].Type, specs[0].Optional, specs[0].Scope
	for _, sp := range specs {
		d.Variants = append(d.Variants, variantView{Systems: systemNames(sp.OS), Command: sp.Command,
			Parse: collect.DescribeParse(sp.Parse), SpecID: sp.ID})
		d.TrySpecs = append(d.TrySpecs, sp.ID)
	}
}

// collectorsFileProblem says what is wrong with the collectors file, if
// anything: either it could not be loaded at start-up, or it has been edited
// since and would not load now. The file is read again on each visit, so a
// mistake shows up here before the next restart turns it into an outage.
func (s *Server) collectorsFileProblem() string {
	path := s.Config.Collectors
	if s.Config.CollectorsErr != "" {
		return path + " was not loaded when the server started, so none of its collectors run: " +
			s.Config.CollectorsErr + ". Fix the file, then restart the server."
	}
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err == nil {
		var set *collect.SpecSet
		if set, err = collect.BuiltinSpecs(); err == nil {
			err = set.Merge(b)
		}
	}
	if err != nil {
		return path + " has changed since the server started and would not load now: " + err.Error() +
			". The server keeps running the collectors it loaded at start-up; a restart with the file as it is would not start."
	}
	return ""
}

// stackNames lists the stacks, with how many hosts are in each.
func (s *Server) stackNames(ctx context.Context) ([]string, map[string]int, error) {
	views, err := s.Store.Stacks(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(views))
	members := map[string]int{}
	for _, v := range views {
		names = append(names, v.Stack.Name)
		members[v.Stack.Name] = v.Health.Members
	}
	return names, members, nil
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return strconv.Itoa(n) + " " + what + "s"
}

func specSystems(specs []collect.Spec) []string {
	var out []string
	for _, sp := range specs {
		for _, o := range sp.OS {
			if !slices.Contains(out, o) {
				out = append(out, o)
			}
		}
	}
	return out
}

// systemNames spells operating system families the way people write them,
// always in the same order.
func systemNames(oss []string) []string {
	rank := map[string]int{"linux": 1, "aix": 2, "windows": 3}
	oss = slices.Clone(oss)
	slices.SortStableFunc(oss, func(a, b string) int {
		ra, rb := rank[strings.ToLower(a)], rank[strings.ToLower(b)]
		if ra == 0 {
			ra = 9
		}
		if rb == 0 {
			rb = 9
		}
		return ra - rb
	})
	out := make([]string, 0, len(oss))
	for _, o := range oss {
		switch strings.ToLower(o) {
		case "linux":
			out = append(out, "Linux")
		case "aix":
			out = append(out, "AIX")
		case "windows":
			out = append(out, "Windows")
		default:
			out = append(out, o)
		}
	}
	return out
}

func (s *Server) handleCollectorGuide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "guide.html", "Writing a collector", "guide", struct {
		Body template.HTML
		TOC  []tocEntry
	}{s.collectorGuide, s.collectorGuideTOC})
}

// handleQueryGuide serves the query language reference.
func (s *Server) handleQueryGuide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "guide.html", "Queries", "guide", struct {
		Body template.HTML
		TOC  []tocEntry
	}{s.queryGuide, s.queryGuideTOC})
}

// renderDocs renders the embedded documents once, at start-up.
func (s *Server) renderDocs() {
	s.guide, s.guideTOC = renderMarkdown(docs.UserGuide)
	s.collectorGuide, s.collectorGuideTOC = renderMarkdown(docs.Collectors)
	s.queryGuide, s.queryGuideTOC = renderMarkdown(docs.Queries)
}
