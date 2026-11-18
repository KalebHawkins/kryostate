package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// DefaultCredentialFailLimit is how many authentication failures in a row put
// a password credential on hold. Directory accounts commonly lock after three
// to five bad passwords; two leaves room for the person's own attempts.
const DefaultCredentialFailLimit = 2

// CredentialHeldError is returned instead of connecting when the host's
// password credential is on hold. Nothing was sent to the host.
type CredentialHeldError struct {
	Hold store.CredentialHold
}

func (e *CredentialHeldError) Error() string {
	h := e.Hold
	last := ""
	if h.LastHost != "" {
		last = fmt.Sprintf(", last on %s", h.LastHost)
	}
	if msg := strings.Join(strings.Fields(h.LastError), " "); msg != "" {
		last += ": " + msg
	}
	return fmt.Sprintf("not tried: %s for %s is on hold after %d authentication failures in a row%s. "+
		"Nothing logs in with it until the password is replaced (%s) or the hold released (kryostate secret release %s)",
		h.Ref, h.Username, h.Fails, last, replaceHint(h.Ref), h.Ref)
}

func replaceHint(ref string) string {
	if v, ok := strings.CutPrefix(ref, "keyring:"); ok {
		return "kryostate secret set " + v
	}
	return "update what " + ref + " resolves to, then release the hold"
}

// guardsPassword reports whether a connection logs in with a password, which
// is what a directory counts against an account. An agent or a key file
// does not lock anybody out.
func guardsPassword(p store.ConnProfile) bool {
	if p.Transport == "winrm" {
		return true
	}
	scheme, _, _ := strings.Cut(p.CredentialRef, ":")
	switch scheme {
	case "agent", "file", "":
		return false
	}
	return true
}

// credGate keeps password attempts in step within one run. Until a
// credential has logged in once in the run, hosts using it take turns, so a
// password that changed since the last run fails once rather than once per
// concurrent session. After a success they connect in parallel again.
type credGate struct {
	mu sync.Mutex
	m  map[string]*credState
}

type credState struct {
	slot chan struct{}
	mu   sync.Mutex
	// provenRun is the run in which the credential last logged in.
	provenRun string
}

func (g *credGate) state(ref, user string) *credState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*credState{}
	}
	k := ref + "\x00" + user
	s, ok := g.m[k]
	if !ok {
		s = &credState{slot: make(chan struct{}, 1)}
		g.m[k] = s
	}
	return s
}

func (s *credState) proven(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return runID != "" && s.provenRun == runID
}

func (s *credState) setProven(runID string) {
	s.mu.Lock()
	s.provenRun = runID
	s.mu.Unlock()
}

// connectGuarded connects as connect does, but never with a password
// credential that is on hold, one attempt at a time until the credential has
// worked in this run, and counting each authentication failure towards a
// hold.
func (e *Engine) connectGuarded(ctx context.Context, runID string, p store.ConnProfile) (collect.Transport, string, error) {
	if !guardsPassword(p) || e.Store == nil {
		return e.connect(ctx, p)
	}
	st := e.creds.state(p.CredentialRef, p.Username)
	if !st.proven(runID) {
		select {
		case st.slot <- struct{}{}:
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
		// Whoever held the turn may have proven the password meanwhile; then
		// there is no reason to keep everyone else waiting behind this host.
		if st.proven(runID) {
			<-st.slot
		} else {
			defer func() { <-st.slot }()
		}
	}
	rec := context.WithoutCancel(ctx)
	if h, ok, err := e.Store.CredentialHoldFor(rec, p.CredentialRef, p.Username); err != nil {
		return nil, "", err
	} else if ok && h.Held {
		return nil, "", &CredentialHeldError{Hold: h}
	}

	t, family, err := e.connect(ctx, p)
	if err == nil {
		if !st.proven(runID) {
			st.setProven(runID)
			if _, cerr := e.Store.ClearCredentialHold(rec, p.CredentialRef, p.Username); cerr != nil {
				e.logf("clear credential failures", cerr)
			}
		}
		return t, family, nil
	}
	if outcomeFor(err) != model.OutcomeAuthFail {
		return nil, "", err
	}
	st.setProven("")
	limit := e.CredentialFailLimit
	if limit <= 0 {
		limit = DefaultCredentialFailLimit
	}
	h, nerr := e.Store.NoteCredentialFailure(rec, p.CredentialRef, p.Username, p.Address, trimErr(err), limit)
	if nerr != nil {
		e.logf("count credential failure", nerr)
		return nil, "", err
	}
	if h.Held {
		_ = e.Store.Audit(rec, "system", "hold credential", p.CredentialRef,
			fmt.Sprintf("%s after %d authentication failures in a row, last on %s", p.Username, h.Fails, p.Address))
		return nil, "", fmt.Errorf("%w; %s for %s is now on hold, so no other host is tried with it (%s)",
			err, p.CredentialRef, p.Username, replaceHint(p.CredentialRef))
	}
	return nil, "", err
}

// isCredentialHeld reports whether err is a connection not attempted because
// its credential is on hold.
func isCredentialHeld(err error) bool {
	var he *CredentialHeldError
	return errors.As(err, &he)
}
