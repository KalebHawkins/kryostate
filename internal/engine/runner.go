// Package engine runs collection and resolution passes and writes what they
// find into the store.
//
// Two rules shape everything here. A failure is recorded, never swallowed:
// "we could not reach this host" is a finding, and a host that silently drops
// out of a run leaves the inventory reporting better coverage than it has. And
// nothing is retried automatically after a timeout, because a local timeout
// does not stop a command that is still running on the far end.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/normalize"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Engine holds the pieces a collection run needs.
type Engine struct {
	Store  *store.Store
	Specs  *collect.SpecSet
	Norm   *normalize.Normalizer
	Secret *secret.Registry
	// Externals are operator-supplied programs run locally, once per host, for
	// values this tool has no built-in way to read.
	Externals []collect.External
	// KeepEvidence stores what each collector command printed, so that a
	// parse rule found to be wrong can be corrected against stored text
	// rather than by connecting to every host again. It is on by default
	// because that repair path is the reason raw text is kept at all;
	// somewhere a command returns more than its caller wants on disk, and
	// that estate turns it off.
	KeepEvidence bool
	// Log is optional. It receives the failures that have nowhere else to go,
	// such as a probe that could not be written.
	Log *slog.Logger
	// Dial replaces the real SSH and WinRM connection, for tests. It returns
	// the transport and the operating system family the host reports.
	Dial func(ctx context.Context, p store.ConnProfile) (collect.Transport, string, error)
}

// Wire gives the store the rules it needs to normalise an application a person
// states. Without it the stated value is charted as one no rule understood.
func (e *Engine) Wire() {
	if e.Store != nil && e.Norm != nil {
		e.Store.SetNormalizer(normAdapter{e.Norm})
	}
}

type normAdapter struct{ n *normalize.Normalizer }

func (a normAdapter) NormalizeFor(attr, value string) (store.Normalized, bool) {
	res, ok := a.n.Normalize(attr, value)
	if !ok {
		return store.Normalized{}, false
	}
	return store.Normalized{
		Value: res.Value, Family: res.Family, Major: res.Major,
		Minor: res.Minor, Patch: res.Patch, RuleID: res.RuleID,
		Version: a.n.Version,
	}, true
}

// Options bound a run. The defaults are conservative because the alternative
// is a tool that opens hundreds of sessions into production at once.
type Options struct {
	// Transport limits the run to one transport, or all when empty.
	Transport string
	// Only limits the run to specific configuration items.
	Only []string
	// Concurrency caps simultaneous hosts.
	Concurrency int
	// HostTimeout bounds everything done against one host.
	HostTimeout time.Duration
	// Actor is recorded in the audit trail.
	Actor string
	// Trigger is recorded on the run, so the runs page can say what started
	// it. Empty means a person at the command line.
	Trigger string
}

func (o *Options) applyDefaults() {
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
	if o.HostTimeout <= 0 {
		o.HostTimeout = 2 * time.Minute
	}
	if o.Actor == "" {
		o.Actor = "system"
	}
}

// Progress reports one host finishing, for a live view of a run.
type Progress struct {
	RunID   string
	CIID    string
	Label   string
	Outcome model.Outcome
	Detail  string
	Facts   int
	Done    int
	Total   int
}

// Collect runs every applicable collector against every enabled target.
//
// The run is recorded before it starts and closed when it ends, so a partial
// pass is distinguishable from a complete one. That distinction is what stops
// a failed run from being read as evidence that software was uninstalled.
func (e *Engine) Collect(ctx context.Context, opt Options, progress chan<- Progress) (store.RunSummary, error) {
	opt.applyDefaults()
	if progress != nil {
		defer close(progress)
	}

	targets, err := e.Store.CollectTargets(ctx, opt.Transport)
	if err != nil {
		return store.RunSummary{}, err
	}
	if len(opt.Only) > 0 {
		keep := map[string]bool{}
		for _, id := range opt.Only {
			keep[id] = true
		}
		var filtered []store.ConnProfile
		for _, t := range targets {
			if keep[t.CIID] {
				filtered = append(filtered, t)
			}
		}
		targets = filtered
	}

	// Where each collector runs is decided once for the whole run.
	tg, err := e.targetsFor(ctx)
	if err != nil {
		return store.RunSummary{}, err
	}

	started := time.Now()
	runID, err := e.Store.StartRunAs(ctx, "builtin", opt.Trigger, len(targets))
	if err != nil {
		return store.RunSummary{}, err
	}
	total := len(targets)

	var (
		mu       sync.Mutex
		okCount  int
		errCount int
		done     int
	)
	sem := make(chan struct{}, opt.Concurrency)
	var wg sync.WaitGroup

	for _, t := range targets {
		wg.Add(1)
		go func(t store.ConnProfile) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			hctx, cancel := context.WithTimeout(ctx, opt.HostTimeout)
			defer cancel()

			label := t.Address
			if ci, err := e.Store.CI(hctx, t.CIID); err == nil && ci.Label != "" {
				label = ci.Label
			}

			n, err := e.collectSome(hctx, runID, t, nil, tg)

			mu.Lock()
			done++
			p := Progress{RunID: runID, CIID: t.CIID, Label: label, Facts: n, Done: done, Total: total}
			if err != nil {
				errCount++
				p.Outcome = outcomeFor(err)
				p.Detail = err.Error()
			} else {
				okCount++
				p.Outcome = model.OutcomeOK
			}
			mu.Unlock()

			if progress != nil {
				select {
				case progress <- p:
				default:
				}
			}
		}(t)
	}
	wg.Wait()

	if len(e.Externals) > 0 {
		exOK, exFailed := e.runExternals(ctx, opt, runID, progress, done)
		okCount += exOK
		errCount += exFailed
		total += exOK + exFailed
	}

	status := "complete"
	if ctx.Err() != nil {
		status = "interrupted"
	}
	closing := context.WithoutCancel(ctx)
	if err := e.Store.FinishRun(closing, runID, status, okCount, errCount); err != nil {
		return store.RunSummary{}, err
	}
	_ = e.Store.Audit(closing, opt.Actor, "collect", runID,
		fmt.Sprintf("%d targets, %d ok, %d failed", total, okCount, errCount))
	if len(opt.Only) == 0 && opt.Transport == "" {
		e.markAllRan(closing, runID, started, status, okCount, errCount)
	}

	return store.RunSummary{
		ID: runID, Collector: "builtin", Status: status,
		Targets: total, Succeeded: okCount, Failed: errCount,
	}, nil
}

// outcomeFor turns a transport error into the specific condition it
// represents. "Could not connect" is several different problems with several
// different owners, and collapsing them wastes the person who has to chase it.
func outcomeFor(err error) model.Outcome {
	switch {
	case err == nil:
		return model.OutcomeOK
	case errors.Is(err, context.DeadlineExceeded):
		return model.OutcomeTimeout
	case errors.Is(err, collect.ErrHostKeyChanged), errors.Is(err, collect.ErrHostKeyUnknown):
		return model.OutcomeError
	}
	s := strings.ToLower(err.Error())
	// Matched as phrases rather than on the substring "auth", which also
	// appears in "certificate signed by unknown authority" — a TLS problem
	// that would otherwise send someone off to rotate a working credential.
	switch {
	case strings.Contains(s, "unable to authenticate"),
		strings.Contains(s, "authentication failed"),
		strings.Contains(s, "permission denied"),
		strings.Contains(s, "no supported methods remain"),
		strings.Contains(s, "http error: 401"):
		return model.OutcomeAuthFail
	case strings.Contains(s, "refused"):
		return model.OutcomeRefused
	case strings.Contains(s, "no such host"):
		return model.OutcomeNXDomain
	case strings.Contains(s, "timeout"), strings.Contains(s, "timed out"), strings.Contains(s, "deadline"):
		return model.OutcomeTimeout
	default:
		return model.OutcomeError
	}
}

// collectSome connects to one host and runs the specs that apply to it. With
// keep nil that is every applicable spec, which is what a full collection
// does. With keep set only the specs it accepts run, for a scheduled run of
// some collectors; then a host with nothing to do, or whose optional commands
// all found nothing, is not a failure, because nothing was asked of it that it
// could fail.
//
// A failure to connect is returned as a *DialError, so a caller can tell an
// unreachable host from one that answered badly.
//
// tg is the run's view of where collectors run; nil loads it for this host
// when some collector needs it.
func (e *Engine) collectSome(ctx context.Context, runID string, p store.ConnProfile, keep func(collect.Spec) bool, tg *Targets) (int, error) {
	// Recording a failure must not be prevented by the deadline that caused
	// it. Without this a timed-out host writes no probe at all and is then
	// reported as one nobody has ever tried to reach.
	rec := context.WithoutCancel(ctx)

	t, osFamily, err := e.connect(ctx, p)
	if err != nil {
		if perr := e.Store.RecordProbe(rec, model.Probe{
			CIID: p.CIID, Kind: probeKindFor(p.Transport), Outcome: outcomeFor(err),
			Target: p.Address, Detail: trimErr(err),
		}); perr != nil {
			e.logProbe(perr)
		}
		if perr := e.Store.RecordProbe(rec, model.Probe{
			CIID: p.CIID, Kind: model.ProbeCollectorRun, Outcome: outcomeFor(err),
			Target: p.Address, Detail: trimErr(err),
		}); perr != nil {
			e.logProbe(perr)
		}
		return 0, &DialError{Err: err}
	}
	defer t.Close()

	_ = e.Store.RecordProbe(rec, model.Probe{
		CIID: p.CIID, Kind: probeKindFor(p.Transport), Outcome: model.OutcomeOK, Target: p.Address,
	})
	if p.HostKey == "" && t.HostKey() != "" {
		_ = e.Store.PinHostKey(ctx, p.CIID, p.Transport, t.HostKey())
	}
	if bk, ok := t.(interface{ BastionHostKey() string }); ok {
		if p.BastionHostKey == "" && bk.BastionHostKey() != "" {
			_ = e.Store.PinBastionHostKey(ctx, p.CIID, p.Transport, bk.BastionHostKey())
		}
	}

	srcID, err := e.Store.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		return 0, err
	}

	specs, err := e.specsForHost(ctx, p.CIID, osFamily, tg)
	if err != nil {
		return 0, err
	}
	if keep != nil {
		specs = slices.DeleteFunc(specs, func(s collect.Spec) bool { return !keep(s) })
		if len(specs) == 0 {
			return 0, nil
		}
	}
	// An optional spec reads something that legitimately may not be there,
	// such as a hardware model inside a container. Its absence is not a
	// failed collection, so it does not turn the run partial.
	optional := map[string]bool{}
	for _, s := range specs {
		if s.Optional {
			optional[s.ID] = true
		}
	}
	readings := collect.Run(ctx, t, specs)
	written := 0
	var failures []string
	for _, r := range readings {
		// Output is kept when the command ran, including when the parse rule
		// then found nothing — that case is precisely what the repair path
		// exists for. Output from a command that failed is not kept: replaying
		// a rule over an error message would manufacture a value.
		if e.KeepEvidence && r.Ran {
			if eerr := e.Store.RecordEvidence(ctx, store.Evidence{
				CIID: p.CIID, SpecID: r.SpecID, Attr: r.Attr, Scope: r.Scope,
				Command: r.Command, Output: r.Output, RunID: runID, At: r.ObservedAt,
			}); eerr != nil && e.Log != nil {
				e.Log.Warn("evidence not recorded", "spec", r.SpecID, "err", eerr)
			}
		}
		if r.Err != nil {
			if !optional[r.SpecID] {
				failures = append(failures, r.SpecID)
			}
			continue
		}
		aid, err := e.assertID(ctx, p.CIID, r, srcID, runID)
		if err != nil {
			return written, err
		}
		written++
		// Linking the output to the claim it produced is what lets a later
		// repair replace its own derivation instead of guessing from whichever
		// source currently wins.
		if e.KeepEvidence && r.Ran {
			_ = e.Store.LinkEvidence(ctx, p.CIID, r.SpecID, r.Scope, aid)
		}

		// Identity evidence is filed as identity, not as a fact about the
		// machine, so that a rebuilt host with a fresh machine-id becomes a
		// question rather than an automatic second record.
		if r.Attr == "host.machine_id" && r.Value != "" {
			_ = e.Store.AddIdentity(ctx, p.CIID, model.IdentMachineID, r.Value)
		}
		if r.Attr == "hw.serial" && r.Value != "" {
			_ = e.Store.AddIdentity(ctx, p.CIID, model.IdentSerial, r.Value)
		}
	}

	detail := ""
	if len(failures) > 0 {
		detail = "no value from: " + strings.Join(failures, ", ")
	}
	// A connection that produced nothing is a failed collection, not a quiet
	// success, and the run must say so rather than counting the host as done.
	outcome := model.OutcomeOK
	var runErr error
	if written == 0 && keep != nil && len(failures) == 0 {
		detail = "nothing found by the optional commands"
	} else if written == 0 {
		outcome = model.OutcomeError
		runErr = fmt.Errorf("connected to %s but no collector returned a value", p.Address)
	} else if len(failures) > 0 {
		outcome = model.OutcomePartial
	}
	_ = e.Store.RecordProbe(rec, model.Probe{
		CIID: p.CIID, Kind: model.ProbeCollectorRun, Outcome: outcome,
		Target: p.Address, Detail: detail,
	})
	return written, runErr
}

func (e *Engine) logProbe(err error) {
	if e.Log != nil {
		e.Log.Warn("probe not recorded", "err", err)
	}
}

func (e *Engine) assert(ctx context.Context, ciID string, r collect.Reading, srcID int64, runID string) error {
	_, err := e.assertID(ctx, ciID, r, srcID, runID)
	return err
}

// assertID files a reading and returns the assertion's id, which the evidence
// link needs so that a later repair can find its own derivation again.
func (e *Engine) assertID(ctx context.Context, ciID string, r collect.Reading, srcID int64, runID string) (int64, error) {
	a := model.Assertion{
		CIID: ciID, Attr: r.Attr, Raw: r.Value, ValueType: r.Type,
		SourceID: srcID, Scope: r.Scope, RunID: runID, SpecID: r.SpecID, ObservedAt: r.ObservedAt,
	}
	var n *store.Normalized
	if res, ok := e.Norm.Normalize(r.Attr, r.Value); ok {
		n = &store.Normalized{
			Value: res.Value, Family: res.Family, Major: res.Major,
			Minor: res.Minor, Patch: res.Patch, RuleID: res.RuleID,
			Version: e.Norm.Version,
		}
	}
	return e.Store.Assert(ctx, a, n)
}

func probeKindFor(transport string) model.ProbeKind {
	if transport == "winrm" {
		return model.ProbeWinRMAuth
	}
	return model.ProbeSSHAuth
}

func (e *Engine) dial(ctx context.Context, p store.ConnProfile) (collect.Transport, string, error) {
	switch p.Transport {
	case "ssh":
		t, err := collect.DialSSH(ctx, collect.SSHConfig{
			Address: p.Address, Port: p.Port, User: p.Username,
			CredentialRef: p.CredentialRef, Bastion: p.BastionRef,
			KnownHostKey: p.HostKey, KnownBastionKey: p.BastionHostKey,
			RequireKnownHostKey: p.HostKeyPolicy == "strict",
		}, e.Secret)
		if err != nil {
			return nil, "", err
		}
		family, err := detectFamily(ctx, t)
		if err != nil {
			t.Close()
			return nil, "", err
		}
		return t, family, nil
	case "winrm":
		t, err := collect.DialWinRM(ctx, collect.WinRMConfig{
			Address: p.Address, Port: p.Port, User: p.Username,
			CredentialRef: p.CredentialRef, HTTPS: true,
		}, e.Secret)
		if err != nil {
			return nil, "", err
		}
		return t, "windows", nil
	default:
		return nil, "", fmt.Errorf("unknown transport %q", p.Transport)
	}
}

// detectFamily asks the host what it is rather than guessing from its name or
// from which collector happened to succeed first.
func detectFamily(ctx context.Context, t collect.Transport) (string, error) {
	res, err := t.Run(ctx, "uname -s")
	if err != nil {
		return "", err
	}
	v := strings.ToLower(strings.TrimSpace(res.Stdout))
	switch {
	case strings.Contains(v, "aix"):
		return "aix", nil
	case strings.Contains(v, "linux"):
		return "linux", nil
	case v == "":
		return "", errors.New("host did not identify its operating system")
	default:
		return v, nil
	}
}

func trimErr(err error) string {
	s := err.Error()
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
