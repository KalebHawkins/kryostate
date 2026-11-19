package web

// Handlers that change the inventory from the browser.
//
// Every one of them is a plain form POST that redirects back to a page, so the
// back button and a page refresh never resubmit anything. The outcome is shown
// once, from a short-lived cookie, rather than carried in the URL where it
// could be forged into a link.
//
// What is deliberately not editable here:
//
//   - Collector specs and external programs. A spec is a command run on every
//     host, and this interface has no authentication of its own.
//   - Connections. If the address of a profile could be changed here, anyone
//     who can reach this page could point it at their own machine and
//     receive the password. They are shown read-only with the command to
//     change them.
//   - Health checks. A check makes the server connect to an address it
//     names, so one stored from here could probe the internal network when
//     checks run. They are added with "kryostate check add".
//   - A host's application, and deleting a host. They are set with
//     "kryostate host update" and "kryostate host rm".

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/KalebHawkins/kryostate/internal/model"
)

const flashCookie = "kryostate_flash"

type flash struct {
	Kind string // "ok" or "error"
	Text string
}

func setFlash(w http.ResponseWriter, kind, text string) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(kind + "|" + text),
		Path: "/", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

// takeFlash reads the pending message, if any, and clears it.
func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return nil
	}
	kind, text, ok := strings.Cut(v, "|")
	if !ok || (kind != "ok" && kind != "error") {
		return nil
	}
	return &flash{Kind: kind, Text: text}
}

// done records a message and sends the browser to a page with GET.
func done(w http.ResponseWriter, r *http.Request, to, msg string) {
	setFlash(w, "ok", msg)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func refuse(w http.ResponseWriter, r *http.Request, to string, err error) {
	setFlash(w, "error", err.Error())
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func hostURL(id string) string { return "/hosts/" + id }

// field reads a trimmed form value.
func field(r *http.Request, name string) string { return strings.TrimSpace(r.FormValue(name)) }

// plainToken rejects whitespace and control characters, for values that end up
// as a username or a network address.
func plainToken(what, v string) error {
	if v == "" {
		return errors.New(what + " is required")
	}
	for _, c := range v {
		if unicode.IsSpace(c) || unicode.IsControl(c) {
			return errors.New(what + " cannot contain spaces")
		}
	}
	return nil
}

// liveCI loads the host named in the path, answering 404 when there is none.
func (s *Server) liveCI(w http.ResponseWriter, r *http.Request) (model.CI, bool) {
	ci, err := s.Store.CI(r.Context(), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return ci, false
	}
	return ci, true
}

func (s *Server) audit(r *http.Request, action, subject, detail string) {
	if err := s.Store.Audit(r.Context(), actorOf(r), action, subject, detail); err != nil {
		s.Log.Error("audit", "action", action, "err", err)
	}
}

// ---------- hosts ----------

func (s *Server) handleCreateHost(w http.ResponseWriter, r *http.Request) {
	name, fqdn := field(r, "name"), field(r, "fqdn")
	if err := plainToken("A host name", name); err != nil {
		refuse(w, r, "/hosts", err)
		return
	}
	ev := []model.Identity{{Scheme: model.IdentHostname, Value: name}}
	if fqdn != "" {
		if err := plainToken("The fqdn", fqdn); err != nil {
			refuse(w, r, "/hosts", err)
			return
		}
		ev = append([]model.Identity{{Scheme: model.IdentFQDN, Value: fqdn}}, ev...)
	}
	id, created, err := s.Store.ResolveCI(r.Context(), model.KindHost, name, ev)
	if err != nil {
		refuse(w, r, "/hosts", err)
		return
	}
	if !created {
		done(w, r, hostURL(id), name+" was already recorded; this is it.")
		return
	}
	s.audit(r, "create host", id, name)
	done(w, r, hostURL(id), name+" added. Next, tell kryostate how to reach it under Connection.")
}

func (s *Server) handleDisableHost(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	changed, err := s.Store.DisableHost(r.Context(), ci.ID, actorOf(r), field(r, "reason"))
	if err != nil {
		refuse(w, r, hostURL(ci.ID), err)
		return
	}
	if !changed {
		done(w, r, hostURL(ci.ID), ci.Label+" was already disabled.")
		return
	}
	done(w, r, hostURL(ci.ID), ci.Label+" disabled. It will not be contacted until it is enabled; its connections are kept as they are.")
}

func (s *Server) handleEnableHost(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	changed, err := s.Store.EnableHost(r.Context(), ci.ID, actorOf(r), field(r, "reason"))
	if err != nil {
		refuse(w, r, hostURL(ci.ID), err)
		return
	}
	if !changed {
		done(w, r, hostURL(ci.ID), ci.Label+" was not disabled.")
		return
	}
	done(w, r, hostURL(ci.ID), ci.Label+" enabled. It is collected through its connections again.")
}

// ---------- stacks ----------

func (s *Server) handleCreateStack(w http.ResponseWriter, r *http.Request) {
	st := model.Stack{Name: field(r, "name"), Description: field(r, "description"),
		Owner: field(r, "owner"), Environment: field(r, "environment")}
	if st.Name == "" {
		refuse(w, r, "/stacks", errors.New("a stack needs a name"))
		return
	}
	if _, err := s.Store.StackID(r.Context(), st.Name); err == nil {
		refuse(w, r, "/stacks", errors.New("there is already a stack named "+st.Name))
		return
	}
	if _, err := s.Store.PutStack(r.Context(), st); err != nil {
		refuse(w, r, "/stacks", err)
		return
	}
	s.audit(r, "create stack", st.Name, st.Description)
	msg := "Stack " + st.Name + " created. Add hosts to it here."
	if strings.TrimSpace(r.FormValue("hosts")) != "" {
		added, err := s.bulkAdd(r, st.Name)
		if err != nil {
			refuse(w, r, stackURL(st.Name), errors.New("the stack was created, but its hosts were not added: "+err.Error()))
			return
		}
		msg = "Stack " + st.Name + " created. " + added
	}
	done(w, r, stackURL(st.Name), msg)
}

func (s *Server) handleEditStack(w http.ResponseWriter, r *http.Request) {
	st := model.Stack{Name: field(r, "stack"), Description: field(r, "description"),
		Owner: field(r, "owner"), Environment: field(r, "environment")}
	if _, err := s.Store.StackID(r.Context(), st.Name); err != nil {
		refuse(w, r, "/stacks", err)
		return
	}
	if _, err := s.Store.PutStack(r.Context(), st); err != nil {
		refuse(w, r, "/stacks", err)
		return
	}
	s.audit(r, "edit stack", st.Name, st.Owner+" "+st.Environment)
	done(w, r, stackBack(field(r, "back"), stackURL(st.Name)), "Stack "+st.Name+" saved.")
}

func (s *Server) handleDeleteStack(w http.ResponseWriter, r *http.Request) {
	name := field(r, "stack")
	if err := s.Store.DeleteStack(r.Context(), name); err != nil {
		refuse(w, r, "/stacks", err)
		return
	}
	s.audit(r, "delete stack", name, "")
	done(w, r, "/stacks", "Stack "+name+" deleted. Its hosts are unchanged.")
}

// handleAddMember adds a host to a stack. It is used from both a stack's
// page, which names the host, and the host page, which names the stack; a
// stack named on the host page that does not exist yet is created.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stack := field(r, "stack")
	if stack == "" {
		refuse(w, r, stackBack(field(r, "back"), "/stacks"), errors.New("name the stack"))
		return
	}
	back := stackBack(field(r, "back"), stackURL(stack))
	ciID, err := s.Store.LookupCI(ctx, field(r, "host"))
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	id, err := s.Store.EnsureStack(ctx, stack)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	if err := s.Store.AddToStack(ctx, id, ciID, field(r, "role")); err != nil {
		refuse(w, r, back, err)
		return
	}
	ci, _ := s.Store.CI(ctx, ciID)
	s.audit(r, "add to stack", ciID, stack)
	done(w, r, back, ci.Label+" added to "+stack+".")
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stack, ciID := field(r, "stack"), field(r, "ci")
	id, err := s.Store.StackID(ctx, stack)
	if err != nil {
		refuse(w, r, stackBack(field(r, "back"), "/stacks"), err)
		return
	}
	back := stackBack(field(r, "back"), stackURL(stack))
	if _, err := s.Store.RemoveFromStack(ctx, id, ciID); err != nil {
		refuse(w, r, back, err)
		return
	}
	s.audit(r, "remove from stack", ciID, stack)
	done(w, r, back, "Removed from "+stack+".")
}
