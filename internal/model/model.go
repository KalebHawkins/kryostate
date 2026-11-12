// Package model holds the types shared across the store, collectors and UI.
//
// The central idea: nothing here has a "current value" field. A configuration
// item is a stable identity, and everything said about it is an Assertion with
// a source and a timestamp. The current value is a query result, not a column.
package model

import "time"

// SourceKind classifies where a claim came from. Precedence between kinds is
// configured per attribute in the store, not hard-coded here.
type SourceKind string

const (
	// SourceDocument is a claim copied from written inventory. It records what
	// someone typed, with no evidence the machine agrees.
	SourceDocument SourceKind = "document"
	// SourceCollector is output read from the machine itself.
	SourceCollector SourceKind = "collector"
	// SourceHuman is a deliberate assertion by a named person, with a reason.
	SourceHuman SourceKind = "human"
	// SourceDerived is inferred from another value, usually by decoding a
	// hostname. It is the weakest kind and is barred outright from some
	// attributes.
	SourceDerived SourceKind = "derived"
	// SourceDNS is the answer a resolver gave, which is an observation about
	// the name service and not an instruction about where to connect.
	SourceDNS SourceKind = "dns"
)

// ValueType describes how a value should be compared and rendered. Collectors
// may declare it; otherwise it is inferred from how the value behaves across
// the estate.
type ValueType string

const (
	TypeText    ValueType = "text"
	TypeVersion ValueType = "version"
	TypePath    ValueType = "path"
	TypeNumber  ValueType = "number"
	TypeFlag    ValueType = "flag"
	TypeList    ValueType = "list"
)

// CIKind separates the things that can be asserted about. A host and the
// service answering on a load-balanced address are not the same subject, and
// conflating them produces facts that are true of neither.
type CIKind string

const (
	KindHost    CIKind = "host"
	KindService CIKind = "service"
)

// CI is a configuration item: an opaque identity that outlives any name or
// address it currently has.
type CI struct {
	ID        string
	Kind      CIKind
	Label     string // display name only, never an identity
	CreatedAt time.Time
	RetiredAt *time.Time
}

// IdentityScheme names a way of recognising a machine across renames.
type IdentityScheme string

const (
	// IdentHostname is the weakest scheme. Names are reused and they lie.
	IdentHostname IdentityScheme = "hostname"
	IdentFQDN     IdentityScheme = "fqdn"
	// IdentMachineID is evidence from the running OS: a machine-id, a
	// partition UUID, a system serial.
	IdentMachineID IdentityScheme = "machine_id"
	IdentSerial    IdentityScheme = "serial"
	// IdentSlot is the position a machine occupies in a deployed system. It
	// survives a rebuild that replaces the machine underneath it.
	IdentSlot IdentityScheme = "slot"
)

// Identity is evidence that a CI is the one we think it is, valid over a
// window. Nothing here is assumed globally unique: hostnames repeat across
// sites and a cloned VM carries its source machine-id.
type Identity struct {
	CIID      string
	Scheme    IdentityScheme
	Value     string
	FirstSeen time.Time
	LastSeen  time.Time
	ValidTo   *time.Time
}

// Assertion is one claim about one attribute of one CI, made by one source at
// one time. Assertions are never updated in place; a later assertion from the
// same source supersedes an earlier one but does not erase it.
type Assertion struct {
	ID        int64
	CIID      string
	Attr      string // namespaced, e.g. "os.version" or "appserver.version"
	Raw       string // exactly what the source said, never normalised
	ValueType ValueType
	SourceID  int64
	// Scope distinguishes assertions about different instances on one host,
	// such as two queue managers. Empty means host-wide.
	Scope string
	RunID string
	// SpecID names the collector command that produced a collected value, so
	// a change can be traced to the collector that saw it. Empty for claims
	// that did not come from a collector spec.
	SpecID     string
	ObservedAt time.Time
	ReceivedAt time.Time
	// Decided marks a value a person chose while settling a disagreement. It
	// outranks the precedence table for this attribute on this CI.
	Decided   bool
	Retracted *time.Time
}

// Fact is a resolved view: the assertion that currently wins for an attribute,
// plus the normalised form and enough provenance to argue with it.
type Fact struct {
	CIID       string
	Attr       string
	Raw        string
	Norm       string
	ValueType  ValueType
	SourceKind SourceKind
	SourceName string
	Scope      string
	ObservedAt time.Time
	// ConfirmedAt is the last time a source stood behind this value, which
	// differs from ObservedAt whenever an unchanged value is re-collected.
	ConfirmedAt time.Time
	Decided     bool
	Contested   bool // another source currently asserts a different value
	Normalizer  int
	AssertionID int64
}

// Age reports how long ago the winning assertion was last confirmed.
func (f Fact) Age(now time.Time) time.Duration { return now.Sub(f.ConfirmedAt) }

// ProbeKind separates the questions a single "last seen" column would merge.
// A host can answer on its service port while refusing SSH, and a load
// balancer can answer while every node behind it is down.
type ProbeKind string

const (
	// ProbeICMP is reserved for results produced elsewhere; no command
	// accepts one yet. This tool does not send echo requests: doing so needs
	// a privilege it should not hold, and a reply proves less than a
	// connection to the port the host exists to serve.
	ProbeICMP         ProbeKind = "icmp"
	ProbeTCP          ProbeKind = "tcp"
	ProbeSSHAuth      ProbeKind = "ssh_auth"
	ProbeWinRMAuth    ProbeKind = "winrm_auth"
	ProbeCollectorRun ProbeKind = "collector_run"
	ProbeResolve      ProbeKind = "resolve"
	// ProbeHealth is a request to an endpoint somebody named, run by
	// "kryostate check".
	ProbeHealth ProbeKind = "health"
)

// Outcome records how a probe ended. A failure is data: "SSH refused for three
// days, the service port answering throughout" is the signal that finds a
// mislabelled host.
type Outcome string

const (
	OutcomeOK       Outcome = "ok"
	OutcomeRefused  Outcome = "refused"
	OutcomeTimeout  Outcome = "timeout"
	OutcomeAuthFail Outcome = "auth_failed"
	OutcomeNXDomain Outcome = "nxdomain"
	OutcomeNoAnswer Outcome = "no_answer"
	// OutcomePartial: the host answered and some collectors came back empty.
	// Distinguished from ok so that a host missing half its attributes is not
	// filed beside one that answered everything.
	OutcomePartial Outcome = "partial"
	OutcomeError   Outcome = "error"
)

// Probe is one attempt to reach a CI in one specific way.
type Probe struct {
	ID      int64
	CIID    string
	Kind    ProbeKind
	Outcome Outcome
	Detail  string
	Target  string
	At      time.Time
}

// Unknown distinguishes the three reasons an attribute has no usable value.
// They need different remediation, so the UI must not merge them.
type Unknown string

const (
	// UnknownNeverCollected means no collector has ever run for this.
	UnknownNeverCollected Unknown = "never_collected"
	// UnknownCollectionFailed means we tried and could not reach it.
	UnknownCollectionFailed Unknown = "collection_failed"
	// UnknownUnparsed means the machine answered and no rule understood it.
	UnknownUnparsed Unknown = "unparsed"
)

// DiscrepancyKind names a class of disagreement worth a person's attention.
type DiscrepancyKind string

const (
	// DiscValueConflict: two sources assert different values for one attribute.
	DiscValueConflict DiscrepancyKind = "value_conflict"
	// DiscUnresolved: a documented host resolves in no zone.
	DiscUnresolved DiscrepancyKind = "unresolved"
	// DiscMultiAddress: a name returns several addresses and none is chosen.
	DiscMultiAddress DiscrepancyKind = "multi_address"
	// DiscUndocumented: a machine answers that no document mentions.
	DiscUndocumented DiscrepancyKind = "undocumented"
	// DiscStale: the winning assertion is older than the attribute's budget.
	DiscStale DiscrepancyKind = "stale"
	// DiscUnparsed: a value was collected that no normaliser rule matched.
	DiscUnparsed DiscrepancyKind = "unparsed"
)

// Disposition is what a person decided about a discrepancy. Recording the
// decision, who made it and why is the audit trail that makes the tool worth
// trusting a second time.
type Disposition string

const (
	DispOpen           Disposition = "open"
	DispConfirmMachine Disposition = "confirm_machine" // the document was stale
	DispKeepDocument   Disposition = "keep_document"   // the collector misread
	DispAccepted       Disposition = "accepted"        // known and tolerated
	// DispLapsed is written by the detector, not by a person, when a
	// disagreement stops being detected. It is kept distinct from
	// DispAccepted so that "a machine noticed it went away" is never read as
	// "someone looked at this and decided to live with it".
	DispLapsed Disposition = "lapsed"
)

// Discrepancy is a disagreement about one attribute of one CI. Finding these
// is the purpose of the tool; the list of hosts supports it.
type Discrepancy struct {
	ID          int64
	CIID        string
	CILabel     string
	Attr        string
	Scope       string
	Kind        DiscrepancyKind
	Summary     string
	Claimed     string
	ClaimedBy   string
	Observed    string
	ObservedBy  string
	DetectedAt  time.Time
	Disposition Disposition
	DisposedBy  string
	DisposedAt  *time.Time
	Note        string
}

// Stack is a deployed system. It is a first-class entity rather than a string
// tag because it carries an owner and a description, and because membership is
// many-to-many: a shared database host serves more than one stack.
type Stack struct {
	ID          int64
	Name        string
	Description string
	Owner       string
	Environment string
}

// StackHealth is a count breakdown, never a single colour. A stack with two
// unreachable members is not red and not green; it is 11 of 14 collected.
type StackHealth struct {
	Members     int
	Collected   int
	Failed      int
	NeverSeen   int
	Discrepancy int
}
