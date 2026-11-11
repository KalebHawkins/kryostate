package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// ResolveOptions configure a name resolution pass.
type ResolveOptions struct {
	// Suffixes are appended to a bare hostname, in order, until one answers.
	// An estate spread over several zones needs this; guessing one zone and
	// calling the rest decommissioned is how a host quietly disappears.
	Suffixes    []string
	Resolver    string
	Concurrency int
	Timeout     time.Duration
}

// ResolveSummary counts what a resolution pass found.
type ResolveSummary struct {
	Checked    int
	Resolved   int
	Unresolved int
	// Failed counts hosts whose lookup never got an answer at all, as
	// distinct from hosts the resolver said it had no record for.
	Failed int
	Multi  int
}

// Resolve looks up every host and records what the name service said.
//
// The answers are stored as observations, not as instructions. When a name
// returns more than one address the tool records all of them and raises the
// ambiguity rather than picking one: an address chosen because it sorts first
// is a decision nobody made, pointed at production.
func (e *Engine) Resolve(ctx context.Context, opt ResolveOptions) (ResolveSummary, error) {
	if opt.Concurrency <= 0 {
		opt.Concurrency = 16
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 5 * time.Second
	}
	if len(opt.Suffixes) == 0 {
		opt.Suffixes = []string{""}
	}

	hosts, err := e.Store.Hosts(ctx, store.HostFilter{})
	if err != nil {
		return ResolveSummary{}, err
	}

	res := net.DefaultResolver
	if opt.Resolver != "" {
		server := opt.Resolver
		if !strings.Contains(server, ":") {
			server = net.JoinHostPort(server, "53")
		}
		res = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: opt.Timeout}
				return d.DialContext(ctx, network, server)
			},
		}
	}

	var (
		mu  sync.Mutex
		sum ResolveSummary
		// answered lists the hosts the resolver actually gave an answer
		// about, positive or negative. Only their findings may be lapsed.
		answered []string
	)
	sem := make(chan struct{}, opt.Concurrency)
	var wg sync.WaitGroup
	pass := time.Now()

	for _, h := range hosts {
		wg.Add(1)
		go func(h store.HostRow) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			names := e.candidateNames(ctx, h, opt.Suffixes)
			addrs, answeredBy, lookupErr := lookupAny(ctx, res, names, opt.Timeout)

			mu.Lock()
			sum.Checked++
			mu.Unlock()

			if answeredBy == "" {
				detail := fmt.Sprintf("no answer under %d suffixes", len(opt.Suffixes))
				if lookupErr != nil {
					detail = lookupErr.Error()
				}
				_ = e.Store.RecordProbe(ctx, model.Probe{
					CIID: h.CI.ID, Kind: model.ProbeResolve, Outcome: dnsOutcome(lookupErr),
					Target: h.CI.Label,
					Detail: detail,
				})
				// Only an answer from the resolver is evidence about the
				// name. If the lookup itself failed we know nothing about
				// this host, and saying it resolves nowhere would be the
				// tool making the same unverified claim it exists to catch.
				if lookupErr != nil {
					mu.Lock()
					sum.Failed++
					mu.Unlock()
					return
				}
				_ = e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
					CIID: h.CI.ID, Kind: model.DiscUnresolved,
					Summary:   h.CI.Label + " is listed but resolves in no zone that was tried",
					Claimed:   h.CI.Label,
					ClaimedBy: "document",
					Observed:  "no answer",
				})
				mu.Lock()
				sum.Unresolved++
				answered = append(answered, h.CI.ID)
				mu.Unlock()
				return
			}

			_ = e.Store.RecordProbe(ctx, model.Probe{
				CIID: h.CI.ID, Kind: model.ProbeResolve, Outcome: model.OutcomeOK,
				Target: answeredBy, Detail: strings.Join(addrs, " "),
			})
			for _, a := range addrs {
				family := "ipv4"
				if strings.Contains(a, ":") {
					family = "ipv6"
				}
				_ = e.Store.RecordAddress(ctx, h.CI.ID, a, family, opt.Resolver, pass)
			}

			mu.Lock()
			sum.Resolved++
			answered = append(answered, h.CI.ID)
			mu.Unlock()

			if len(addrs) > 1 {
				existing, _ := e.Store.Addresses(ctx, h.CI.ID)
				chosen := false
				for _, a := range existing {
					chosen = chosen || a.Selected
				}
				if !chosen {
					_ = e.Store.RaiseDiscrepancy(ctx, model.Discrepancy{
						CIID: h.CI.ID, Kind: model.DiscMultiAddress,
						Summary: fmt.Sprintf("%s answers with %d addresses and none has been chosen for management",
							h.CI.Label, len(addrs)),
						Observed:   strings.Join(addrs, ", "),
						ObservedBy: "dns",
					})
					mu.Lock()
					sum.Multi++
					mu.Unlock()
				}
			}
		}(h)
	}
	wg.Wait()

	// A name that now resolves should stop being reported as unresolved, but
	// the row stays so that the history of it having been missing survives.
	// Hosts whose lookup failed outright are left alone: this pass learned
	// nothing about them, and a resolver outage must not read as a recovery.
	if err := e.Store.ResolveStaleFor(ctx, model.DiscUnresolved, pass, answered); err != nil {
		return sum, err
	}
	return sum, nil
}

// candidateNames builds the names to try: any fully qualified name already
// recorded, then the label under each configured suffix.
func (e *Engine) candidateNames(ctx context.Context, h store.HostRow, suffixes []string) []string {
	var names []string
	if ids, err := e.Store.Identities(ctx, h.CI.ID); err == nil {
		for _, id := range ids {
			if id.Scheme == model.IdentFQDN && id.Value != "" {
				names = append(names, id.Value)
			}
		}
	}
	for _, s := range suffixes {
		if s == "" {
			names = append(names, h.CI.Label)
			continue
		}
		names = append(names, h.CI.Label+"."+strings.TrimPrefix(s, "."))
	}
	return names
}

// lookupAny tries each candidate name and reports which one answered. Which
// zone a name resolves in is itself a finding, so the answering name is
// returned rather than assumed to be the first one tried.
//
// The error is returned as well as the addresses, because "this name does not
// exist" and "the resolver did not answer in two seconds" are different
// problems for different people, and collapsing them sends somebody to argue
// with DNS about a host that is fine.
func lookupAny(ctx context.Context, res *net.Resolver, names []string, timeout time.Duration) ([]string, string, error) {
	var firstErr error
	for _, n := range names {
		if n == "" {
			continue
		}
		c, cancel := context.WithTimeout(ctx, timeout)
		addrs, err := res.LookupHost(c, n)
		cancel()
		if err == nil && len(addrs) > 0 {
			return dedupe(addrs), n, nil
		}
		// A name that does not exist is the expected answer for most of the
		// candidates, so it never displaces a real fault.
		if err != nil && !isNotFound(err) {
			if firstErr == nil || isFatal(err) {
				firstErr = err
			}
		}
	}
	return nil, "", firstErr
}

// isNotFound reports the ordinary negative answer: the resolver was reached
// and said no such name.
func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

// isFatal reports a failure of the lookup itself rather than of the name. A
// timeout, a cancelled context or an unreachable resolver all mean the
// question was never answered.
func isFatal(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsTimeout || dnsErr.IsTemporary || !dnsErr.IsNotFound
	}
	return true
}

// dnsOutcome classifies a failed lookup for the probe record.
func dnsOutcome(err error) model.Outcome {
	switch {
	case err == nil:
		return model.OutcomeNXDomain
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return model.OutcomeTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsTimeout {
		return model.OutcomeTimeout
	}
	return model.OutcomeError
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
