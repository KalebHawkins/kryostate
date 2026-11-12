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
//   - Connections that use a password or a helper program. If the address of
//     such a profile could be changed here, anyone who can reach this page
//     could point it at their own machine and receive the password. An
//     ssh-agent connection has no such exposure: the key never leaves the
//     agent. Those profiles are shown read-only with the command to change
//     them.

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// webCredential is the only credential reference the browser may assign.
const webCredential = "agent:"

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

func (s *Server) handleRetire(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	reason := field(r, "reason")
	if reason == "" {
		refuse(w, r, hostURL(ci.ID), errors.New("say why the host is being retired"))
		return
	}
	if err := s.Store.Retire(r.Context(), ci.ID, actorOf(r), reason); err != nil {
		refuse(w, r, hostURL(ci.ID), err)
		return
	}
	done(w, r, hostURL(ci.ID), ci.Label+" retired. Its history is kept and it will not be collected.")
}

func (s *Server) handleUnretire(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	if err := s.Store.Unretire(r.Context(), ci.ID, actorOf(r), field(r, "reason")); err != nil {
		refuse(w, r, hostURL(ci.ID), err)
		return
	}
	done(w, r, hostURL(ci.ID), ci.Label+" restored. Re-enable its connection to collect from it again.")
}

// ---------- connection ----------

// webEditable reports whether the browser may change a profile. See the
// comment at the top of this file.
func webEditable(p store.ConnProfile) bool {
	return p.Transport == "ssh" && p.CredentialRef == webCredential && p.BastionRef == ""
}

func (s *Server) handleConnection(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	back := hostURL(ci.ID)
	profiles, err := s.Store.ConnProfiles(r.Context(), ci.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, p := range profiles {
		if p.Transport == "ssh" && !webEditable(p) {
			refuse(w, r, back, errors.New("this connection uses a credential the web interface cannot change; use the command line"))
			return
		}
	}

	addr, user := field(r, "address"), field(r, "user")
	if err := plainToken("The address", addr); err != nil {
		refuse(w, r, back, err)
		return
	}
	if err := plainToken("The user", user); err != nil {
		refuse(w, r, back, err)
		return
	}
	port := 0
	if v := field(r, "port"); v != "" {
		if port, err = strconv.Atoi(v); err != nil || port < 0 || port > 65535 {
			refuse(w, r, back, errors.New("the port must be a number from 1 to 65535, or blank for 22"))
			return
		}
	}
	p := store.ConnProfile{
		CIID: ci.ID, Transport: "ssh", Address: addr, Port: port, Username: user,
		CredentialRef: webCredential, Enabled: r.FormValue("enabled") != "",
	}
	if err := s.Store.PutConnProfile(r.Context(), p); err != nil {
		refuse(w, r, back, err)
		return
	}
	s.audit(r, "connection", ci.ID, "ssh "+user+"@"+addr)
	msg := "Connection saved."
	if p.Enabled {
		msg += " Start a collection from the Overview page to try it."
	}
	done(w, r, back, msg)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	back := hostURL(ci.ID)
	transport := field(r, "transport")
	profiles, err := s.Store.ConnProfiles(r.Context(), ci.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, p := range profiles {
		if p.Transport == transport && !webEditable(p) {
			refuse(w, r, back, errors.New("this connection can only be removed from the command line"))
			return
		}
	}
	if err := s.Store.DeleteConnProfile(r.Context(), ci.ID, transport); err != nil {
		refuse(w, r, back, err)
		return
	}
	s.audit(r, "delete connection", ci.ID, transport)
	done(w, r, back, "Connection removed.")
}

// ---------- checks ----------

func (s *Server) handleAddCheck(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	back := hostURL(ci.ID)
	c := store.Check{
		CIID: ci.ID, Name: field(r, "name"), Kind: field(r, "kind"),
		Target: field(r, "target"), Expect: field(r, "expect"),
		Timeout: 5 * time.Second, Enabled: true,
	}
	if err := s.Store.PutCheck(r.Context(), c); err != nil {
		refuse(w, r, back, err)
		return
	}
	s.audit(r, "check", ci.ID, c.Name+" "+c.Kind+" "+c.Target)
	done(w, r, back, "Check "+c.Name+" saved. It runs with the next collection.")
}

func (s *Server) handleDeleteCheck(w http.ResponseWriter, r *http.Request) {
	ci, ok := s.liveCI(w, r)
	if !ok {
		return
	}
	name := field(r, "name")
	if err := s.Store.DeleteCheck(r.Context(), ci.ID, name); err != nil {
		refuse(w, r, hostURL(ci.ID), err)
		return
	}
	s.audit(r, "delete check", ci.ID, name)
	done(w, r, hostURL(ci.ID), "Check "+name+" removed.")
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
	msg := "Stack " + st.Name + " created. Add hosts to it below."
	if strings.TrimSpace(r.FormValue("hosts")) != "" {
		added, err := s.bulkAdd(r, st.Name)
		if err != nil {
			refuse(w, r, "/stacks#"+stackAnchor(st.Name), errors.New("the stack was created, but its hosts were not added: "+err.Error()))
			return
		}
		msg = "Stack " + st.Name + " created. " + added
	}
	done(w, r, "/stacks#"+stackAnchor(st.Name), msg)
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
	done(w, r, "/stacks#"+stackAnchor(st.Name), "Stack "+st.Name+" saved.")
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

// handleAddMember adds a host to a stack. It is used from both the stacks
// page, which names the host, and the host page, which names the stack; a
// stack named on the host page that does not exist yet is created.
func (s *Server) handleAddMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := field(r, "back")
	if back != "/stacks" && !strings.HasPrefix(back, "/hosts/") {
		back = "/stacks"
	}
	stack := field(r, "stack")
	if stack == "" {
		refuse(w, r, back, errors.New("name the stack"))
		return
	}
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
	if back == "/stacks" {
		back += "#" + stackAnchor(stack)
	}
	done(w, r, back, ci.Label+" added to "+stack+".")
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := field(r, "back")
	if back != "/stacks" && !strings.HasPrefix(back, "/hosts/") {
		back = "/stacks"
	}
	stack, ciID := field(r, "stack"), field(r, "ci")
	id, err := s.Store.StackID(ctx, stack)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	if _, err := s.Store.RemoveFromStack(ctx, id, ciID); err != nil {
		refuse(w, r, back, err)
		return
	}
	s.audit(r, "remove from stack", ciID, stack)
	done(w, r, back, "Removed from "+stack+".")
}

// stackAnchor is the fragment id of a stack's card.
func stackAnchor(name string) string {
	return "stack-" + slug(name, map[string]int{})
}
