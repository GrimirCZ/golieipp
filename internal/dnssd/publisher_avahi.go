//go:build linux && avahi

package dnssd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/grimir/golieipp/internal/config"
)

const (
	avahiBusName       = "org.freedesktop.Avahi"
	avahiServerPath    = dbus.ObjectPath("/")
	avahiServerIface   = "org.freedesktop.Avahi.Server"
	avahiEntryGroupInt = "org.freedesktop.Avahi.EntryGroup"

	avahiInterfaceUnspec  = int32(-1)
	avahiProtocolUnspec   = int32(-1)
	avahiGroupEstablished = int32(2)
	avahiGroupCollision   = int32(3)
	avahiGroupFailure     = int32(4)

	avahiStatePollInterval = 25 * time.Millisecond
	avahiStateWait         = 2 * time.Second
	avahiCleanupTimeout    = 2 * time.Second
)

// avahiPublisher keeps all daemon interaction behind the Publisher seam. The
// system bus is opened lazily, so an unavailable DBus/Avahi daemon degrades a
// publication rather than preventing proxy startup.
type avahiPublisher struct {
	mu      sync.Mutex
	cfg     config.DNSSDConfig
	logger  *slog.Logger
	conn    *dbus.Conn
	group   dbus.ObjectPath
	current *Publication
	closed  bool
	// freeGroupFn is nil in production and exists as a narrow DBus cleanup
	// seam for deterministic lifecycle tests.
	freeGroupFn func(context.Context, dbus.ObjectPath) error
	// ensureBusFn and updateServiceTXTFn are nil in production. They isolate
	// the DBus boundary for deterministic transaction tests without changing
	// the public Publisher API.
	ensureBusFn        func() error
	updateServiceTXTFn func(context.Context, ServiceRecord) error
	createGroupFn      func(context.Context, ServiceInput, []ServiceRecord) (dbus.ObjectPath, bool, error)
}

func newPlatformPublisher(cfg config.DNSSDConfig, logger *slog.Logger) Publisher {
	return &avahiPublisher{cfg: cfg, logger: logger}
}

func (p *avahiPublisher) Publish(ctx context.Context, input ServiceInput) (Status, error) {
	return p.Update(ctx, input)
}

func (p *avahiPublisher) Update(ctx context.Context, input ServiceInput) (Status, error) {
	if err := contextErr(ctx); err != nil {
		return Status{State: StateDegraded, Name: input.Name, Err: err}, err
	}
	if input.Hostname == "" {
		input.Hostname = p.cfg.Hostname
	}
	if input.Interface == "" {
		input.Interface = p.cfg.Interface
	}
	records, err := BuildRecords(input)
	if err != nil {
		return Status{State: StateDegraded, Name: input.Name, Err: err}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		err := errors.New("DNS-SD publisher is closed")
		return Status{State: StateDegraded, Name: input.Name, Err: err}, err
	}
	if p.cfg.Mode == config.DNSModeOff {
		if p.group != "" {
			if err := p.freeGroupBounded(ctx, p.group); err != nil {
				name := input.Name
				if p.current != nil {
					name = p.current.Name
				}
				if isStaleEntryGroupError(err) {
					// A gone object already satisfies the disabled state. Drop
					// the unusable handle so a later mode change can publish a
					// fresh group; retain the daemon error for diagnostics.
					p.group = ""
					p.current = nil
					return Status{State: StateDisabled, Name: name, Err: err}, nil
				}
				return Status{State: StateDegraded, Name: name, Err: err}, nil
			}
		}
		p.group = ""
		p.current = nil
		return Status{State: StateDisabled, Name: input.Name}, nil
	}
	if p.current != nil {
		input.Name = p.current.Name
		// LOC and endpoint records are established with the EntryGroup. Runtime
		// refreshes update TXT atomically; structural DNS changes take effect on
		// publisher restart rather than leaving the in-memory snapshot ahead of
		// what Avahi actually serves.
		input.Hostname = p.current.Input.Hostname
		input.Port = p.current.Input.Port
		input.IPPS = p.current.Input.IPPS
		input.Interface = p.current.Input.Interface
		input.GeoLocation = p.current.Input.GeoLocation
	}
	if err := p.ensureBus(); err != nil {
		return Status{State: StateDegraded, Name: input.Name, Err: err}, nil
	}
	if p.group != "" && p.current != nil {
		// Service identity and endpoint are configuration-owned and stable for
		// the lifetime of a publisher. Capability refreshes only replace TXT.
		// Avahi requires UpdateServiceTxt on the existing EntryGroup; the
		// AVAHI_PUBLISH_UPDATE flag is not a cross-group replacement primitive.
		records, err = BuildRecords(input)
		if err != nil {
			return Status{State: StateDegraded, Name: p.current.Name, Err: err}, nil
		}
		if err := p.updateGroupTXT(ctx, input, records); err != nil {
			currentName := input.Name
			if p.current != nil {
				currentName = p.current.Name
			}
			if isStaleEntryGroupError(err) {
				// Avahi object paths do not survive a daemon restart. Do not
				// retain an invalid path as the current publication: the next
				// retry must be able to create a fresh entry group.
				p.group = ""
				p.current = nil
				return Status{State: StateDegraded, Name: currentName, Err: err}, nil
			}
			if p.group != "" && p.current != nil && p.conn != nil {
				// A daemon restart invalidates object paths and a runtime collision
				// removes the publication. Use a bounded, independent context here:
				// the failed update's caller context may already be canceled, while
				// this state check and cleanup still need to finish safely.
				stateCtx, cancel := context.WithTimeout(context.Background(), avahiStateWait)
				group := p.conn.Object(avahiBusName, p.group)
				var state int32
				stateErr := group.CallWithContext(stateCtx, avahiEntryGroupInt+".GetState", 0).Store(&state)
				cancel()
				if stateErr != nil || state != avahiGroupEstablished {
					// Keep the old snapshot if bounded cleanup cannot complete; Close
					// can retry Free and no object path is silently leaked.
					if cleanupErr := p.freeGroupBounded(context.Background(), p.group); cleanupErr == nil || isStaleEntryGroupError(stateErr) || isStaleEntryGroupError(cleanupErr) {
						p.group = ""
						p.current = nil
					}
				}
			}
			return Status{State: StateDegraded, Name: currentName, Err: err}, nil
		}
		p.current = &Publication{Name: input.Name, Input: cloneInput(input), Records: cloneRecords(records)}
		return Status{State: StatePublished, Name: input.Name, Attempts: 1}, nil
	}
	maxAttempts := input.MaxCollisionRetries
	if maxAttempts == 0 {
		maxAttempts = DefaultCollisionRetries
	} else {
		maxAttempts++
	}
	for attempt, candidate := range CollisionNames(input.Name, maxAttempts) {
		candidateInput := cloneInput(input)
		candidateInput.Name = candidate
		candidateRecords := cloneRecords(records)
		for index := range candidateRecords {
			candidateRecords[index].Name = candidate
		}
		group, collision, err := p.createEntryGroup(ctx, candidateInput, candidateRecords)
		if collision {
			continue
		}
		if err != nil {
			return Status{State: StateDegraded, Name: candidate, Attempts: attempt + 1, Err: err}, nil
		}
		p.group = group
		p.current = &Publication{Name: candidate, Input: candidateInput, Records: candidateRecords}
		return Status{State: StatePublished, Name: candidate, Attempts: attempt + 1}, nil
	}
	err = errors.New("DNS-SD service name collision")
	return Status{State: StateDegraded, Name: input.Name, Attempts: maxAttempts, Err: err}, nil
}

func (p *avahiPublisher) Withdraw(ctx context.Context) (Status, error) {
	if err := contextErr(ctx); err != nil {
		return Status{State: StateDegraded, Err: err}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	name := ""
	if p.current != nil {
		name = p.current.Name
	}
	if p.group == "" {
		p.current = nil
		return Status{State: StateWithdrawn, Name: name}, nil
	}
	if err := p.freeGroupBounded(ctx, p.group); err != nil {
		if isStaleEntryGroupError(err) {
			// The daemon already discarded this object. Treat withdrawal as
			// complete and forget the unusable path; retain the error in the
			// status for callers that want to diagnose the daemon transition.
			p.group = ""
			p.current = nil
			return Status{State: StateWithdrawn, Name: name, Err: err}, nil
		}
		return Status{State: StateDegraded, Name: name, Err: err}, nil
	}
	p.group = ""
	p.current = nil
	return Status{State: StateWithdrawn, Name: name}, nil
}

func (p *avahiPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.group != "" {
		if err := p.freeGroupBounded(context.Background(), p.group); err != nil {
			if !isStaleEntryGroupError(err) {
				return err
			}
			// The daemon has already discarded the object, so shutdown has
			// achieved its desired state. Do not leave an unusable handle that
			// prevents Close from completing on a later call.
		}
		p.group = ""
		p.current = nil
	}
	p.closed = true
	// SystemBus is a shared godbus connection and must not be closed by an
	// individual queue publisher.
	p.conn = nil
	return nil
}

func (p *avahiPublisher) ensureBus() error {
	if p.ensureBusFn != nil {
		return p.ensureBusFn()
	}
	if p.conn != nil && p.conn.Connected() {
		return nil
	}
	conn, err := dbus.SystemBus()
	if err != nil {
		return err
	}
	p.conn = conn
	return nil
}

func (p *avahiPublisher) createGroup(ctx context.Context, input ServiceInput, records []ServiceRecord) (dbus.ObjectPath, bool, error) {
	server := p.conn.Object(avahiBusName, avahiServerPath)
	var path dbus.ObjectPath
	if err := server.CallWithContext(ctx, avahiServerIface+".EntryGroupNew", 0).Store(&path); err != nil {
		return "", isCollisionError(err), err
	}
	group := p.conn.Object(avahiBusName, path)
	cleanup := true
	defer func() {
		if cleanup {
			_ = p.freeGroupBounded(context.Background(), path)
		}
	}()
	interfaceIndex, err := interfaceIndex(input.Interface)
	if err != nil {
		return "", false, err
	}
	flags := uint32(0)
	for _, record := range records {
		if record.Subtype != "" {
			if err := group.CallWithContext(ctx, avahiEntryGroupInt+".AddServiceSubtype", 0,
				interfaceIndex, avahiProtocolUnspec, flags, record.Name, record.Type, "", record.FullType()).Err; err != nil {
				return "", isCollisionError(err), err
			}
			continue
		}
		if err := group.CallWithContext(ctx, avahiEntryGroupInt+".AddService", 0,
			interfaceIndex, avahiProtocolUnspec, flags, record.Name, record.Type, "", record.Hostname, record.Port, avahiTXT(record.TXT)).Err; err != nil {
			return "", isCollisionError(err), err
		}
		if record.GeoLocation != "" && record.Hostname != "" {
			loc, err := EncodeLOC(record.GeoLocation)
			if err != nil {
				return "", false, err
			}
			// LOC is attached to the advertised host, not to each service
			// instance. BuildRecords carries it only on the first service.
			if err := group.CallWithContext(ctx, avahiEntryGroupInt+".AddRecord", 0,
				interfaceIndex, avahiProtocolUnspec, flags, record.Hostname, uint16(1), uint16(29), uint32(120), loc).Err; err != nil {
				return "", isCollisionError(err), err
			}
		}
	}
	if err := group.CallWithContext(ctx, avahiEntryGroupInt+".Commit", 0).Err; err != nil {
		return "", isCollisionError(err), err
	}
	state, err := waitForGroupState(ctx, group, path)
	if err != nil {
		return "", false, err
	}
	if state == avahiGroupCollision {
		return "", true, fmt.Errorf("avahi reported service name collision for %q", input.Name)
	}
	if state != avahiGroupEstablished {
		return "", false, fmt.Errorf("avahi entry group entered state %d", state)
	}
	cleanup = false
	return path, false, nil
}

func (p *avahiPublisher) createEntryGroup(ctx context.Context, input ServiceInput, records []ServiceRecord) (dbus.ObjectPath, bool, error) {
	if p.createGroupFn != nil {
		return p.createGroupFn(ctx, input, records)
	}
	return p.createGroup(ctx, input, records)
}

type avahiTXTUpdateError struct {
	updateErr   error
	rollbackErr error
}

func (e *avahiTXTUpdateError) Error() string {
	if e.rollbackErr == nil {
		return e.updateErr.Error()
	}
	return fmt.Sprintf("TXT update failed: %v; rollback failed: %v", e.updateErr, e.rollbackErr)
}

func (e *avahiTXTUpdateError) Unwrap() error {
	return e.updateErr
}

func (p *avahiPublisher) updateGroupTXT(ctx context.Context, input ServiceInput, records []ServiceRecord) error {
	interfaceIndex, err := interfaceIndex(input.Interface)
	if err != nil {
		return err
	}
	previous := make(map[string]ServiceRecord)
	if p.current != nil {
		for _, record := range p.current.Records {
			if record.Subtype == "" {
				previous[record.Type] = record
			}
		}
	}
	updated := make([]ServiceRecord, 0, len(records))
	for _, record := range records {
		// The legacy _printer service is deliberately published without TXT;
		// it has no capability data to refresh. Subtype records inherit TXT
		// from their parent and are not independently updated by Avahi.
		if record.Subtype != "" || record.Type == "_printer._tcp" {
			continue
		}
		if err := p.updateServiceTXT(ctx, interfaceIndex, record); err != nil {
			// An error can mean that Avahi applied the update before the DBus
			// reply was lost. Include the failed record in rollback, not just
			// the records that returned success.
			updated = append(updated, record)
			rollbackErr := p.rollbackTXT(updated, previous, interfaceIndex)
			if rollbackErr != nil {
				// A partially restored EntryGroup must never be retained as the
				// current publication. Free it with the existing bounded cleanup
				// path; on success the next retry will rebuild it from scratch.
				if cleanupErr := p.freeGroupBounded(context.Background(), p.group); cleanupErr == nil {
					p.group = ""
					p.current = nil
				} else {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("free failed group: %w", cleanupErr))
				}
				return &avahiTXTUpdateError{updateErr: err, rollbackErr: rollbackErr}
			}
			return err
		}
		updated = append(updated, record)
	}
	return nil
}

func (p *avahiPublisher) updateServiceTXT(ctx context.Context, interfaceIndex int32, record ServiceRecord) error {
	if p.updateServiceTXTFn != nil {
		return p.updateServiceTXTFn(ctx, record)
	}
	group := p.conn.Object(avahiBusName, p.group)
	return group.CallWithContext(ctx, avahiEntryGroupInt+".UpdateServiceTxt", 0,
		interfaceIndex, avahiProtocolUnspec, uint32(0), record.Name, record.Type, "", avahiTXT(record.TXT)).Err
}

func (p *avahiPublisher) rollbackTXT(updated []ServiceRecord, previous map[string]ServiceRecord, interfaceIndex int32) error {
	ctx, cancel := context.WithTimeout(context.Background(), avahiCleanupTimeout)
	defer cancel()
	for _, record := range updated {
		old, ok := previous[record.Type]
		if !ok {
			return fmt.Errorf("no previous TXT record for %s", record.Type)
		}
		if err := p.updateServiceTXT(ctx, interfaceIndex, old); err != nil {
			return fmt.Errorf("restore %s TXT: %w", record.Type, err)
		}
	}
	return nil
}

func (p *avahiPublisher) freeGroup(ctx context.Context, path dbus.ObjectPath) error {
	if p.freeGroupFn != nil {
		return p.freeGroupFn(ctx, path)
	}
	if p.conn == nil || path == "" {
		return nil
	}
	return p.conn.Object(avahiBusName, path).CallWithContext(ctx, avahiEntryGroupInt+".Free", 0).Err
}

func (p *avahiPublisher) freeGroupBounded(parent context.Context, path dbus.ObjectPath) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, avahiCleanupTimeout)
	defer cancel()
	return p.freeGroup(ctx, path)
}

func waitForGroupState(ctx context.Context, group dbus.BusObject, path dbus.ObjectPath) (int32, error) {
	deadlineCtx, cancel := context.WithTimeout(ctx, avahiStateWait)
	defer cancel()
	ticker := time.NewTicker(avahiStatePollInterval)
	defer ticker.Stop()
	for {
		var state int32
		if err := group.CallWithContext(deadlineCtx, avahiEntryGroupInt+".GetState", 0).Store(&state); err != nil {
			return 0, err
		}
		switch state {
		case avahiGroupEstablished, avahiGroupCollision, avahiGroupFailure:
			return state, nil
		}
		select {
		case <-deadlineCtx.Done():
			return 0, fmt.Errorf("waiting for avahi entry group %s: %w", path, deadlineCtx.Err())
		case <-ticker.C:
		}
	}
}

func interfaceIndex(name string) (int32, error) {
	if name == "" {
		return avahiInterfaceUnspec, nil
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return int32(iface.Index), nil
}

func avahiTXT(txt map[string]string) [][]byte {
	entries := TXTEntries(txt)
	values := make([][]byte, len(entries))
	for index, entry := range entries {
		values[index] = []byte(entry)
	}
	return values
}

func isCollisionError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "collision") || strings.Contains(message, "nameconflict") || strings.Contains(message, "already exists")
}

func isStaleEntryGroupError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unknownobject") ||
		strings.Contains(message, "unknown object") ||
		strings.Contains(message, "no such object") ||
		strings.Contains(message, "serviceunknown") ||
		strings.Contains(message, "service unknown") ||
		strings.Contains(message, "entry group no longer exists")
}
