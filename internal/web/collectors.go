package web

import (
	"html/template"
	"net/http"
	"path/filepath"

	"github.com/KalebHawkins/hearsay/docs"
	"github.com/KalebHawkins/hearsay/internal/collect"
)

// ConfigFiles says where this process looks for the files a person writes,
// and which of them it found. The collectors page shows it, because "where do
// I put my file" is the first question anyone adding a collector asks.
type ConfigFiles struct {
	Dir        string
	Collectors string // path loaded, or blank
	Rules      string
	Externals  string
}

type collectorsData struct {
	Config   ConfigFiles
	Builtin  []collect.Spec
	Mine     []collect.Spec
	External []collect.External
	// WantPath is where a new collectors file goes.
	WantPath string

	Catalog        []catalogGroup
	On             []choiceRow
	Stacks         []string
	Hosts          []hostOption // hosts that can be tested on: those with a connection
	AllowRevealing bool
}

// handleCollectors lists what runs against hosts. It is read-only on purpose:
// a collector is a command run on every host, and this interface has no
// authentication of its own.
func (s *Server) handleCollectors(w http.ResponseWriter, r *http.Request) {
	d := collectorsData{Config: s.Config, WantPath: s.Config.Collectors}
	if d.WantPath == "" && s.Config.Dir != "" {
		d.WantPath = s.Config.Dir + string(filepath.Separator) + "collectors.json"
	}
	if s.Engine != nil && s.Engine.Specs != nil {
		builtin, err := collect.BuiltinSpecs()
		if err != nil {
			s.fail(w, err)
			return
		}
		isBuiltin := map[string]bool{}
		for _, sp := range builtin.Specs {
			isBuiltin[sp.ID] = true
		}
		for _, sp := range s.Engine.Specs.Specs {
			if isBuiltin[sp.ID] {
				d.Builtin = append(d.Builtin, sp)
			} else {
				d.Mine = append(d.Mine, sp)
			}
		}
		d.External = s.Engine.Externals
	}
	var err error
	if d.Catalog, d.On, d.Stacks, err = s.catalogData(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	d.AllowRevealing = s.AllowRevealing
	if targets, err := s.Store.CollectTargets(r.Context(), ""); err == nil {
		seen := map[string]bool{}
		for _, p := range targets {
			if c, err := s.Store.CI(r.Context(), p.CIID); err == nil && !seen[c.ID] {
				seen[c.ID] = true
				d.Hosts = append(d.Hosts, hostOption{ID: c.ID, Label: c.Label})
			}
		}
	}
	s.render(w, r, "collectors.html", "Collectors", "collectors", d)
}

func (s *Server) handleCollectorGuide(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "guide.html", "Writing a collector", "guide", struct {
		Body template.HTML
		TOC  []tocEntry
	}{s.collectorGuide, s.collectorGuideTOC})
}

// renderDocs renders the embedded documents once, at start-up.
func (s *Server) renderDocs() {
	s.guide, s.guideTOC = renderMarkdown(docs.UserGuide)
	s.collectorGuide, s.collectorGuideTOC = renderMarkdown(docs.Collectors)
}

type hostOption struct{ ID, Label string }
