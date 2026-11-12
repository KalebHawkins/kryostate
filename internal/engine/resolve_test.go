package engine

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// TestLookupFailureIsNotAMissingRecord covers the distinction the tool exists
// to make. "No such name" is a finding about the estate. "The resolver did
// not answer" is a finding about the resolver, and reporting it as the former
// sends somebody to look for a host that is fine.
func TestLookupFailureIsNotAMissingRecord(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want model.Outcome
	}{
		{"no answer from the resolver at all", nil, model.OutcomeNXDomain},
		{"deadline", context.DeadlineExceeded, model.OutcomeTimeout},
		{"cancelled", context.Canceled, model.OutcomeTimeout},
		{"dns timeout", &net.DNSError{Err: "timeout", IsTimeout: true}, model.OutcomeTimeout},
		{"resolver unreachable", &net.DNSError{Err: "connection refused"}, model.OutcomeError},
	}
	for _, c := range cases {
		if got := dnsOutcome(c.err); got != c.want {
			t.Errorf("%s: dnsOutcome = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestNotFoundDoesNotMaskARealFault checks which error survives a run through
// several candidate names. Most candidates are expected to not exist, so an
// NXDOMAIN must never displace the timeout that means the question was never
// answered.
func TestNotFoundDoesNotMaskARealFault(t *testing.T) {
	notFound := &net.DNSError{Err: "no such host", IsNotFound: true}
	if !isNotFound(notFound) {
		t.Fatal("an IsNotFound DNSError is not being recognised as a negative answer")
	}
	if isFatal(notFound) {
		t.Error("a name that does not exist is being treated as a lookup failure")
	}
	for _, err := range []error{
		context.DeadlineExceeded,
		&net.DNSError{Err: "timeout", IsTimeout: true},
		&net.DNSError{Err: "server misbehaving", IsTemporary: true},
		errors.New("something else entirely"),
	} {
		if !isFatal(err) {
			t.Errorf("%v is being treated as a definitive negative answer", err)
		}
		if isNotFound(err) {
			t.Errorf("%v is being read as no-such-name", err)
		}
	}
}

// TestLookupAnyReportsWhyItGotNothing makes sure the error reaches the caller
// rather than being flattened into an empty result.
func TestLookupAnyReportsWhyItGotNothing(t *testing.T) {
	refusing := &net.Resolver{
		PreferGo: true,
		Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("no resolver reachable")
		},
	}
	addrs, answered, err := lookupAny(context.Background(), refusing,
		[]string{"a.invalid", "b.invalid"}, 0)
	if len(addrs) != 0 || answered != "" {
		t.Fatalf("a failing resolver produced an answer: %v via %q", addrs, answered)
	}
	if err == nil {
		t.Fatal("a resolver that could not be reached reported no error, so the host would be filed as resolving nowhere")
	}
}

// TestAResolverOutageIsNotAnAnswer is the end-to-end form of the rule above. A
// pass that could not ask the question must not answer it: a host whose lookup
// failed is counted as failed, never as resolving nowhere.
func TestAResolverOutageIsNotAnAnswer(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	host(t, st, "node-outage")

	// Today's pass cannot reach a resolver at all.
	sum, err := e.Resolve(ctx, ResolveOptions{
		Resolver: "127.0.0.1:1", Timeout: time.Second, Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || sum.Unresolved != 0 {
		t.Fatalf("a dead resolver was not counted as a failed lookup: %+v", sum)
	}
}
