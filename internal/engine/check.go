package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// CheckResult is one check's outcome, returned so a caller can print it.
type CheckResult struct {
	Check   store.Check
	Outcome model.Outcome
	Detail  string
	Took    time.Duration
}

// RunChecks runs every enabled check and records each as a probe.
//
// These are deliberately not pings. An ICMP echo needs a privilege this tool
// should not hold, and answers a question nobody has: a host that replies to
// ping while the service it exists to run is refusing connections is the case
// this is for. A TCP connect, or a request to the health endpoint somebody
// already wrote, says something.
//
// Nothing is retried. A check that failed once failed once, and smoothing that
// away is how a record of reachability becomes a record of nothing.
func (e *Engine) RunChecks(ctx context.Context, only []string, concurrency int) ([]CheckResult, error) {
	checks, err := e.Store.AllChecks(ctx)
	if err != nil {
		return nil, err
	}
	if len(only) > 0 {
		keep := map[string]bool{}
		for _, id := range only {
			keep[id] = true
		}
		var f []store.Check
		for _, c := range checks {
			if keep[c.CIID] {
				f = append(f, c)
			}
		}
		checks = f
	}
	if concurrency <= 0 {
		concurrency = 16
	}

	out := make([]CheckResult, len(checks))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c store.Check) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = e.runCheck(ctx, c)
		}(i, c)
	}
	wg.Wait()

	// Written after the run rather than inside it, so a single connection
	// pool is not contended by every worker at once.
	rec := context.WithoutCancel(ctx)
	for _, r := range out {
		kind := model.ProbeTCP
		if r.Check.Kind == "http" {
			kind = model.ProbeHealth
		}
		if err := e.Store.RecordProbe(rec, model.Probe{
			CIID: r.Check.CIID, Kind: kind, Outcome: r.Outcome,
			Target: r.Check.Name + " " + r.Check.Target, Detail: r.Detail,
		}); err != nil {
			e.logProbe(err)
		}
	}
	return out, nil
}

func (e *Engine) runCheck(ctx context.Context, c store.Check) CheckResult {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()

	start := time.Now()
	r := CheckResult{Check: c, Outcome: model.OutcomeOK}
	var err error
	switch c.Kind {
	case "tcp":
		err = checkTCP(ctx, c.Target)
	case "http":
		r.Detail, err = checkHTTP(ctx, c.Target, c.Expect)
	default:
		err = fmt.Errorf("unknown check kind %q", c.Kind)
	}
	r.Took = time.Since(start)
	if err != nil {
		r.Outcome = outcomeFor(err)
		r.Detail = trimErr(err)
	}
	return r
}

func checkTCP(ctx context.Context, target string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return err
	}
	return conn.Close()
}

// checkHTTP reports the status it got as well as whether it was wanted, so
// "503 since Tuesday" and "connection refused since Tuesday" stay apart.
func checkHTTP(ctx context.Context, target, expect string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "kryostate")
	resp, err := checkClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	detail := "status " + strconv.Itoa(resp.StatusCode)
	if !statusMatches(resp.StatusCode, expect) {
		return detail, fmt.Errorf("http error: %d, wanted %s", resp.StatusCode, expect)
	}
	return detail, nil
}

// checkClient does not follow redirects. A health endpoint that redirects to a
// login page is not healthy, and following it would report the login page's
// 200 as the service being up.
var checkClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Timeout:       30 * time.Second,
}

func statusMatches(code int, expect string) bool {
	got := strconv.Itoa(code)
	if len(expect) != 3 || len(got) != 3 {
		return false
	}
	for i := range expect {
		if expect[i] == 'x' || expect[i] == 'X' {
			continue
		}
		if expect[i] != got[i] {
			return false
		}
	}
	return true
}

// CheckSummary counts results for a one-line report.
func CheckSummary(rs []CheckResult) (ok, failed int) {
	for _, r := range rs {
		if r.Outcome == model.OutcomeOK {
			ok++
		} else {
			failed++
		}
	}
	return
}
