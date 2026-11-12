package ingest

import (
	"context"
	"fmt"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// HostLine is one host typed or pasted into a list.
type HostLine struct {
	Line int
	Name string
	FQDN string
	Role string
}

// ParseHostList reads one host per line, in the forms people paste:
//
//	web01
//	web01 app
//	web01 app web01.example.com
//	web01.example.com database
//	web01:app
//
// Words may be separated by spaces, tabs or commas. A word containing a dot is
// the fully qualified name; when the line starts with one, the short name is
// its first label. Any other word is the role. Blank lines and lines starting
// with # are skipped.
func ParseHostList(text string) ([]HostLine, []string) {
	var out []HostLine
	var problems []string
	for i, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words := strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ';'
		})
		if len(words) == 1 {
			if name, role, ok := strings.Cut(words[0], ":"); ok {
				words = []string{name, role}
			}
		}
		h := HostLine{Line: i + 1}
		var roles []string
		for _, w := range words {
			switch {
			case strings.Contains(w, ".") && h.FQDN == "":
				h.FQDN = strings.ToLower(w)
			case h.Name == "" && len(roles) == 0 && h.FQDN == "":
				h.Name = w
			default:
				roles = append(roles, w)
			}
		}
		if h.Name == "" && h.FQDN != "" {
			h.Name, _, _ = strings.Cut(h.FQDN, ".")
		}
		if h.Name == "" {
			problems = append(problems, fmt.Sprintf("line %d: no host name", i+1))
			continue
		}
		h.Role = strings.Join(roles, " ")
		out = append(out, h)
	}
	return out, problems
}

// BulkOptions controls AddHostsToStack.
type BulkOptions struct {
	// Create adds hosts that are not recorded yet. Without it a name that
	// matches nothing is reported as a problem.
	Create bool
	// User, when set, gives every host without an ssh connection one that
	// logs in as this user through ssh-agent.
	User string
	By   string
}

// BulkResult counts what AddHostsToStack did.
type BulkResult struct {
	Added     int
	Created   int
	Connected int
	Problems  []string
}

// AddHostsToStack puts each listed host in a stack, creating the stack if it
// is new. A line that fails is reported and the rest still go in: one typo in
// a list of forty should not cost the other thirty-nine.
func AddHostsToStack(ctx context.Context, st *store.Store, stack string, hosts []HostLine, opt BulkOptions) (BulkResult, error) {
	var res BulkResult
	stackID, err := st.EnsureStack(ctx, stack)
	if err != nil {
		return res, err
	}
	for _, h := range hosts {
		var id string
		if opt.Create {
			ev := []model.Identity{{Scheme: model.IdentHostname, Value: h.Name}}
			if h.FQDN != "" {
				ev = append([]model.Identity{{Scheme: model.IdentFQDN, Value: h.FQDN}}, ev...)
			}
			var created bool
			id, created, err = st.ResolveCI(ctx, model.KindHost, h.Name, ev)
			if created {
				res.Created++
				_ = st.Audit(ctx, opt.By, "create host", id, h.Name)
			}
		} else {
			ref := h.Name
			if h.FQDN != "" {
				ref = h.FQDN
			}
			id, err = st.LookupCI(ctx, ref)
		}
		if err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("line %d (%s): %v", h.Line, h.Name, err))
			continue
		}
		if err := st.AddToStack(ctx, stackID, id, h.Role); err != nil {
			return res, err
		}
		res.Added++

		if opt.User != "" {
			profiles, err := st.ConnProfiles(ctx, id)
			if err != nil {
				return res, err
			}
			hasSSH := false
			for _, p := range profiles {
				hasSSH = hasSSH || p.Transport == "ssh"
			}
			if !hasSSH {
				addr := h.FQDN
				if addr == "" {
					addr = h.Name
				}
				if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh",
					Address: addr, Username: opt.User, CredentialRef: "agent:", Enabled: true}); err != nil {
					return res, err
				}
				res.Connected++
			}
		}
	}
	_ = st.Audit(ctx, opt.By, "add hosts to stack", stack, fmt.Sprintf("%d added, %d created", res.Added, res.Created))
	return res, nil
}

// Summary is one sentence describing a result.
func (r BulkResult) Summary(stack string) string {
	noun := "hosts"
	if r.Added == 1 {
		noun = "host"
	}
	s := fmt.Sprintf("%d %s added to %s", r.Added, noun, stack)
	var extra []string
	if r.Created > 0 {
		extra = append(extra, fmt.Sprintf("%d of them new", r.Created))
	}
	if r.Connected > 0 {
		extra = append(extra, fmt.Sprintf("%d given a connection", r.Connected))
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	s += "."
	if len(r.Problems) > 0 {
		s += " Not added: " + strings.Join(r.Problems, "; ") + "."
	}
	return s
}
