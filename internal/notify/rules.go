package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// RulesSource is the digest's Rules section: progress on every open rollout,
// and the rules that finished in the window. Only movement counts as news, so
// a rule that sits unchanged does not make an otherwise quiet day worth a
// message.
func RulesSource(st *store.Store) SectionSource {
	return func(ctx context.Context, since, until time.Time) (Section, error) {
		rs, err := st.RuleStatus(ctx)
		if err != nil {
			return Section{}, err
		}
		sec := Section{Key: store.SectionRules, Title: "Rules"}
		moved := func(t time.Time) bool { return !t.IsZero() && t.After(since) && !t.After(until) }
		for _, r := range rs {
			switch {
			case r.Err != "":
				sec.Lines = append(sec.Lines, fmt.Sprintf("%s: the rule no longer parses (%s)", r.Rule.Name, r.Err))
			case r.Complete:
				if moved(r.CompleteSince) {
					sec.Lines = append(sec.Lines, fmt.Sprintf("%s: complete, all %d hosts done", r.Rule.Name, r.InScope))
					sec.Count++
				}
			default:
				line := fmt.Sprintf("%s: %d of %d done (%d%%)", r.Rule.Name, r.Done, r.InScope, r.Pct())
				if r.NotYet > 0 {
					line += fmt.Sprintf(", %d not yet", r.NotYet)
				}
				if r.Unknown > 0 {
					line += fmt.Sprintf(", %d unknown", r.Unknown)
				}
				if moved(r.LastChange) {
					line += " (moved since the last digest)"
					sec.Count++
				}
				sec.Lines = append(sec.Lines, line)
			}
		}
		return sec, nil
	}
}
