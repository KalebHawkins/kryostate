package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// externalFile is the operator-supplied list of programs to run.
type externalFile struct {
	Version   int             `json:"version"`
	Externals []externalEntry `json:"externals"`
}

type externalEntry struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	Comment string   `json:"comment,omitempty"`
}

// LoadExternals reads a file describing programs that report facts this tool
// has no built-in way to read.
//
// The file names programs on the local disk. Anyone who can write it can run
// code as this process, which is the same privilege as editing the command
// specs, and is why neither is fetched from anywhere.
func LoadExternals(path string) ([]collect.External, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f externalFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if f.Version < 1 {
		return nil, fmt.Errorf("%s needs a version of 1 or more", path)
	}
	seen := map[string]bool{}
	out := make([]collect.External, 0, len(f.Externals))
	for i, e := range f.Externals {
		switch {
		case e.Name == "":
			return nil, fmt.Errorf("external %d has no name", i)
		case seen[e.Name]:
			return nil, fmt.Errorf("duplicate external collector %q", e.Name)
		case e.Path == "":
			return nil, fmt.Errorf("external %s has no path", e.Name)
		}
		seen[e.Name] = true
		ex := collect.External{Name: e.Name, Path: e.Path, Args: e.Args, Env: e.Env}
		if e.Timeout != "" {
			d, err := time.ParseDuration(e.Timeout)
			if err != nil {
				return nil, fmt.Errorf("external %s: %w", e.Name, err)
			}
			ex.Timeout = d
		}
		for _, kv := range e.Env {
			if !strings.Contains(kv, "=") {
				return nil, fmt.Errorf("external %s: env entry %q is not name=value", e.Name, kv)
			}
		}
		out = append(out, ex)
	}
	return out, nil
}

// runExternals runs every configured program against every host in scope.
//
// It is a separate pass rather than part of the transport pass because an
// external collector does not need a connection profile. A host this tool
// cannot log into may still be described by a program that queries something
// else, and refusing to ask would make the tool's reach the limit of what can
// be known.
func (e *Engine) runExternals(ctx context.Context, opt Options, runID string, progress chan<- Progress, base int) (ok, failed int) {
	hosts, err := e.externalTargets(ctx, opt)
	if err != nil {
		e.logf("external targets", err)
		return 0, 0
	}

	var (
		mu   sync.Mutex
		done = base
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, opt.Concurrency)

	for _, h := range hosts {
		for _, ex := range e.Externals {
			wg.Add(1)
			go func(h externalTarget, ex collect.External) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				hctx, cancel := context.WithTimeout(ctx, opt.HostTimeout)
				defer cancel()

				n, err := e.runExternalOne(hctx, runID, h, ex)

				mu.Lock()
				done++
				p := Progress{
					RunID: runID, CIID: h.ID, Label: h.Label + " (" + ex.Name + ")",
					Facts: n, Done: done,
				}
				if err != nil {
					failed++
					p.Outcome = outcomeFor(err)
					p.Detail = err.Error()
				} else {
					ok++
					p.Outcome = model.OutcomeOK
				}
				mu.Unlock()

				if progress != nil {
					select {
					case progress <- p:
					default:
					}
				}
			}(h, ex)
		}
	}
	wg.Wait()
	return ok, failed
}

type externalTarget struct {
	ID      string
	Label   string
	Address string
}

func (e *Engine) externalTargets(ctx context.Context, opt Options) ([]externalTarget, error) {
	rows, err := e.Store.Hosts(ctx, store.HostFilter{})
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, id := range opt.Only {
		keep[id] = true
	}
	var out []externalTarget
	for _, r := range rows {
		if len(keep) > 0 && !keep[r.CI.ID] {
			continue
		}
		out = append(out, externalTarget{ID: r.CI.ID, Label: r.CI.Label, Address: e.addressFor(ctx, r.CI.ID)})
	}
	return out, nil
}

// addressFor gives an external collector somewhere to point. A chosen address
// wins; otherwise the most recently observed one is offered, and a collector
// that needs more than a name can say so by failing.
func (e *Engine) addressFor(ctx context.Context, ciID string) string {
	addrs, err := e.Store.Addresses(ctx, ciID)
	if err != nil {
		return ""
	}
	best := ""
	for _, a := range addrs {
		if a.Selected {
			return a.Addr
		}
		if best == "" {
			best = a.Addr
		}
	}
	return best
}

func (e *Engine) runExternalOne(ctx context.Context, runID string, h externalTarget, ex collect.External) (int, error) {
	rec := context.WithoutCancel(ctx)

	env, err := collect.RunExternal(ctx, ex, h.Label, h.Address)
	if err != nil {
		if perr := e.Store.RecordProbe(rec, model.Probe{
			CIID: h.ID, Kind: model.ProbeCollectorRun, Outcome: outcomeFor(err),
			Target: ex.Name, Detail: trimErr(err),
		}); perr != nil {
			e.logProbe(perr)
		}
		return 0, fmt.Errorf("%s on %s: %w", ex.Name, h.Label, err)
	}
	if env.Status != "ok" {
		detail := env.Error
		if detail == "" {
			detail = "collector reported failure without saying why"
		}
		if perr := e.Store.RecordProbe(rec, model.Probe{
			CIID: h.ID, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeError,
			Target: ex.Name, Detail: truncate(detail, 300),
		}); perr != nil {
			e.logProbe(perr)
		}
		return 0, fmt.Errorf("%s on %s: %s", ex.Name, h.Label, detail)
	}

	srcID, err := e.Store.SourceID(ctx, model.SourceCollector, "external:"+ex.Name)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, f := range env.Facts {
		at := time.Now()
		if f.ObservedAt != nil {
			at = *f.ObservedAt
		}
		r := collect.Reading{
			SpecID: ex.Name, Attr: f.Key, Scope: f.Scope,
			Value: f.Value, Type: f.Type, ObservedAt: at,
		}
		if err := e.assert(ctx, h.ID, r, srcID, runID); err != nil {
			return written, err
		}
		written++
	}
	_ = e.Store.RecordProbe(rec, model.Probe{
		CIID: h.ID, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeOK,
		Target: ex.Name, Detail: fmt.Sprintf("%d facts", written),
	})
	return written, nil
}

func (e *Engine) logf(what string, err error) {
	if e.Log != nil && err != nil && !errors.Is(err, context.Canceled) {
		e.Log.Warn(what, "err", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
