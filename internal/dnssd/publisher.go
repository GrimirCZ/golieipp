package dnssd

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/grimir/golieipp/internal/config"
)

// Config is the DNS-SD configuration section used by NewPublisher.
type Config = config.DNSSDConfig

type State string

const (
	StatePublished   State = "published"
	StateDegraded    State = "degraded"
	StateWithdrawn   State = "withdrawn"
	StateDisabled    State = "disabled"
	StateUnpublished State = "unpublished"

	StatusPublished = StatePublished
	StatusDegraded  = StateDegraded
	StatusWithdrawn = StateWithdrawn
)

// Status is deliberately independent of IPP capabilities. A degraded status
// means the publication could not be synchronized with the local DNS-SD
// daemon; it is operational state, not a proxy startup error.
type Status struct {
	State    State
	Name     string
	Attempts int
	Err      error
}

// PublishStatus is an expressive alias for callers that prefer the longer
// name in status plumbing.
type PublishStatus = Status

// Publication is a snapshot of the last successful publication.
type Publication struct {
	Name    string
	Input   ServiceInput
	Records []ServiceRecord
}

// DebugState is a diagnostic snapshot of the publisher-owned DNS-SD state.
// It is intentionally richer than the public readiness summary: the proxy
// uses it only for an operator-requested signal dump.
//
// HasPublication means that the publisher has a last-known-good publication
// snapshot. It does not prove that multicast packets are currently visible to
// a client on the network.
type DebugState struct {
	Backend            string
	Availability       string
	EntryGroup         string
	EntryGroupState    string
	EntryGroupStateErr string
	Closed             bool
	HasPublication     bool
	Publication        Publication
}

// Publisher is the seam consumed by the proxy lifecycle. Implementations must
// make Update transactional from the caller's perspective: a failed update
// leaves the prior publication in place and reports degraded state.
type Publisher interface {
	Publish(context.Context, ServiceInput) (Status, error)
	Update(context.Context, ServiceInput) (Status, error)
	Withdraw(context.Context) (Status, error)
	Close() error
}

// DiagnosticPublisher is an optional extension used by the compile-time mDNS
// diagnostic plugin. Keeping it separate from Publisher lets a lightweight or
// third-party runtime publisher participate in publication without having to
// implement operator-only state reporting.
type DiagnosticPublisher interface {
	DebugState() DebugState
}

// NewPublisher selects Avahi on Linux builds carrying the avahi tag and a
// safe stub elsewhere. A logger is optional; omitting it uses slog.Default.
func NewPublisher(cfg config.DNSSDConfig, logger ...*slog.Logger) Publisher {
	selectedLogger := slog.Default()
	if len(logger) > 0 && logger[0] != nil {
		selectedLogger = logger[0]
	}
	return newPlatformPublisher(cfg, selectedLogger)
}

// StubPublisher is used on platforms without the Avahi build tag and is also
// useful as a deterministic test double. It retains the last good publication
// when availability is toggled off, matching the degraded-loss contract.
type StubPublisher struct {
	mu         sync.RWMutex
	available  bool
	disabled   bool
	current    *Publication
	collisions map[string]bool
	logger     *slog.Logger
}

func NewStubPublisher() *StubPublisher {
	return &StubPublisher{available: true, collisions: make(map[string]bool), logger: slog.Default()}
}

func (p *StubPublisher) SetAvailable(available bool) {
	p.mu.Lock()
	p.available = available
	p.mu.Unlock()
}

// SetCollidingNames causes candidate service names to report a collision.
// This is a test-only control but remains harmless for embedders using the
// stub as a local fallback.
func (p *StubPublisher) SetCollidingNames(names ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.collisions = make(map[string]bool, len(names))
	for _, name := range names {
		p.collisions[name] = true
	}
}

func (p *StubPublisher) Publish(ctx context.Context, input ServiceInput) (Status, error) {
	return p.Update(ctx, input)
}

func (p *StubPublisher) Update(ctx context.Context, input ServiceInput) (Status, error) {
	if err := contextErr(ctx); err != nil {
		return Status{State: StateDegraded, Name: input.Name, Err: err}, err
	}
	records, err := BuildRecords(input)
	if err != nil {
		return Status{State: StateDegraded, Name: input.Name, Err: err}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		p.current = nil
		return Status{State: StateDisabled, Name: input.Name}, nil
	}
	if !p.available {
		return Status{State: StateDegraded, Name: input.Name, Err: errors.New("DNS-SD publisher unavailable")}, nil
	}
	maxAttempts := input.MaxCollisionRetries
	if maxAttempts == 0 {
		maxAttempts = DefaultCollisionRetries
	} else {
		maxAttempts++
	}
	for attempt, candidate := range CollisionNames(input.Name, maxAttempts) {
		if p.collisions[candidate] {
			continue
		}
		publication := &Publication{Name: candidate, Input: cloneInput(input), Records: cloneRecords(records)}
		publication.Input.Name = candidate
		for index := range publication.Records {
			publication.Records[index].Name = candidate
		}
		p.current = publication
		return Status{State: StatePublished, Name: candidate, Attempts: attempt + 1}, nil
	}
	return Status{State: StateDegraded, Name: input.Name, Attempts: maxAttempts, Err: errors.New("DNS-SD service name collision")}, nil
}

func (p *StubPublisher) Withdraw(ctx context.Context) (Status, error) {
	if err := contextErr(ctx); err != nil {
		return Status{State: StateDegraded, Err: err}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disabled {
		p.current = nil
		return Status{State: StateDisabled}, nil
	}
	if !p.available {
		name := ""
		if p.current != nil {
			name = p.current.Name
		}
		return Status{State: StateDegraded, Name: name, Err: errors.New("DNS-SD publisher unavailable")}, nil
	}
	name := ""
	if p.current != nil {
		name = p.current.Name
	}
	p.current = nil
	return Status{State: StateWithdrawn, Name: name}, nil
}

func (p *StubPublisher) Close() error {
	_, err := p.Withdraw(context.Background())
	return err
}

func (p *StubPublisher) Current() (Publication, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.current == nil {
		return Publication{}, false
	}
	return *clonePublication(p.current), true
}

func (p *StubPublisher) DebugState() DebugState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	availability := "unavailable"
	if p.disabled {
		availability = "disabled"
	} else if p.available {
		availability = "available"
	}
	state := DebugState{
		Backend:         "stub",
		Availability:    availability,
		EntryGroupState: "not_applicable",
	}
	if p.current != nil {
		state.HasPublication = true
		state.Publication = *clonePublication(p.current)
	}
	return state
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func cloneInput(input ServiceInput) ServiceInput {
	input.TXT = cloneTXT(input.TXT)
	return input
}

func cloneRecords(records []ServiceRecord) []ServiceRecord {
	if len(records) == 0 {
		return nil
	}
	copyRecords := make([]ServiceRecord, len(records))
	for index, record := range records {
		copyRecords[index] = record
		copyRecords[index].TXT = cloneTXT(record.TXT)
	}
	return copyRecords
}

func clonePublication(publication *Publication) *Publication {
	if publication == nil {
		return nil
	}
	copyPublication := *publication
	copyPublication.Input = cloneInput(publication.Input)
	copyPublication.Records = cloneRecords(publication.Records)
	return &copyPublication
}
