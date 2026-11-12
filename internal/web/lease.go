package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// otherCollection says, in a sentence, what collection another process
// sharing this database is running, or "" when none is.
func (s *Server) otherCollection(ctx context.Context) string {
	if s.Scheduler == nil {
		return ""
	}
	l, ok, err := s.Scheduler.Lock().Elsewhere(ctx)
	if err != nil {
		s.Log.Warn("collection lease", "err", err)
		return ""
	}
	if !ok {
		return ""
	}
	return leaseSentence(l)
}

func leaseSentence(l store.Lease) string {
	return fmt.Sprintf("%s is running (since %s). Collect now, and this server's schedule, wait until it finishes.",
		capitalise(l.Describe()), l.AcquiredAt.Local().Format(time.TimeOnly))
}

func capitalise(s string) string {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return s
	}
	return string(s[0]-'a'+'A') + s[1:]
}

// takeLease takes the collection lease for a run started from the web. It
// returns the sentence to show instead when another process holds it.
func (s *Server) takeLease(ctx context.Context, actor string) (func(), string, error) {
	if s.Scheduler == nil {
		return func() {}, "", nil
	}
	release, err := s.Scheduler.Lock().TryAcquire(ctx, engine.LeaseInfo{
		Purpose: "Collect now by " + actor, Origin: "web", Actor: actor})
	var held *engine.LeaseHeldError
	if errors.As(err, &held) {
		return nil, leaseSentence(held.Lease), nil
	}
	return release, "", err
}

func noticeHTML(msg string) string {
	return `<p class="notice" role="status">` + template.HTMLEscapeString(msg) + `</p>`
}
