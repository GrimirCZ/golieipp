//go:build linux && avahi

package dnssd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/grimir/golieipp/internal/config"
)

func TestAvahiCloseBoundsFreeContext(t *testing.T) {
	called := false
	publisher := &avahiPublisher{
		group: dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		freeGroupFn: func(ctx context.Context, _ dbus.ObjectPath) error {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok {
				return fmt.Errorf("free context has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > avahiCleanupTimeout+100*time.Millisecond {
				return fmt.Errorf("free deadline is not bounded: %s", remaining)
			}
			return nil
		},
	}

	if err := publisher.Close(); err != nil {
		t.Fatalf("Close returned an error: %v", err)
	}
	if !called {
		t.Fatal("Close did not free the active entry group")
	}
}

func TestAvahiUpdateRollsBackTXTRecordsOnLaterFailure(t *testing.T) {
	oldInput := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	oldRecords, err := BuildRecords(oldInput)
	if err != nil {
		t.Fatalf("BuildRecords(old): %v", err)
	}

	var calls []ServiceRecord
	updateErr := errors.New("simulated later TXT update failure")
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeAuto},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: oldInput.Name, Input: oldInput, Records: oldRecords},
		ensureBusFn: func() error {
			return nil
		},
	}
	publisher.updateServiceTXTFn = func(_ context.Context, record ServiceRecord) error {
		calls = append(calls, record)
		if record.Type == "_ipps._tcp" && record.TXT["note"] == "new" {
			return updateErr
		}
		return nil
	}

	status, publishErr := publisher.Update(context.Background(), ServiceInput{
		Name:     oldInput.Name,
		Hostname: oldInput.Hostname,
		Port:     oldInput.Port,
		IPPS:     true,
		TXT:      map[string]string{"note": "new"},
	})
	if publishErr != nil {
		t.Fatalf("Update returned transport error: %v", publishErr)
	}
	if status.State != StateDegraded {
		t.Fatalf("Update state = %v, want degraded", status.State)
	}
	if len(calls) != 4 {
		t.Fatalf("UpdateServiceTxt calls = %d, want failed update plus rollback of both records; calls=%v", len(calls), calls)
	}
	if calls[0].Type != "_ipp._tcp" || calls[0].TXT["note"] != "new" {
		t.Fatalf("first TXT update = %#v, want new _ipp TXT", calls[0])
	}
	if calls[1].Type != "_ipps._tcp" || calls[1].TXT["note"] != "new" {
		t.Fatalf("failed TXT update = %#v, want new _ipps TXT", calls[1])
	}
	if calls[2].Type != "_ipp._tcp" || calls[2].TXT["note"] != "old" {
		t.Fatalf("first rollback = %#v, want old _ipp TXT", calls[2])
	}
	if calls[3].Type != "_ipps._tcp" || calls[3].TXT["note"] != "old" {
		t.Fatalf("second rollback = %#v, want old _ipps TXT", calls[3])
	}
	if got := publisher.current.Input.TXT["note"]; got != "old" {
		t.Fatalf("current TXT = %q, want old publication retained", got)
	}
}

func TestAvahiUpdateCleansUpWhenTXTRollbackFails(t *testing.T) {
	oldInput := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	oldRecords, err := BuildRecords(oldInput)
	if err != nil {
		t.Fatalf("BuildRecords(old): %v", err)
	}

	updateErr := errors.New("simulated later TXT update failure")
	rollbackErr := errors.New("simulated TXT rollback failure")
	freeCalled := false
	freeGroupDeadline := time.Time{}
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeAuto},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: oldInput.Name, Input: oldInput, Records: oldRecords},
		ensureBusFn: func() error {
			return nil
		},
		freeGroupFn: func(ctx context.Context, _ dbus.ObjectPath) error {
			freeCalled = true
			freeGroupDeadline, _ = ctx.Deadline()
			return nil
		},
	}
	publisher.updateServiceTXTFn = func(_ context.Context, record ServiceRecord) error {
		if record.Type == "_ipps._tcp" && record.TXT["note"] == "new" {
			return updateErr
		}
		if record.TXT["note"] == "old" {
			return rollbackErr
		}
		return nil
	}

	status, publishErr := publisher.Update(context.Background(), ServiceInput{
		Name:     oldInput.Name,
		Hostname: oldInput.Hostname,
		Port:     oldInput.Port,
		IPPS:     true,
		TXT:      map[string]string{"note": "new"},
	})
	if publishErr != nil {
		t.Fatalf("Update returned transport error: %v", publishErr)
	}
	if status.State != StateDegraded {
		t.Fatalf("Update state = %v, want degraded", status.State)
	}
	if !freeCalled {
		t.Fatal("expected failed rollback to free the group")
	}
	if freeGroupDeadline.IsZero() {
		t.Fatal("expected bounded context for failed-rollback cleanup")
	}
	if remaining := time.Until(freeGroupDeadline); remaining <= 0 || remaining > avahiCleanupTimeout+100*time.Millisecond {
		t.Fatalf("Free cleanup deadline is not bounded to %s: remaining %s", avahiCleanupTimeout, remaining)
	}
	if publisher.group != "" || publisher.current != nil {
		t.Fatalf("failed rollback left stale publication state: group=%q current=%#v", publisher.group, publisher.current)
	}
}

func TestAvahiUpdateClearsInvalidEntryGroupForRetry(t *testing.T) {
	oldInput := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	oldRecords, err := BuildRecords(oldInput)
	if err != nil {
		t.Fatalf("BuildRecords(old): %v", err)
	}

	staleErr := errors.New("org.freedesktop.DBus.Error.UnknownObject: entry group no longer exists")
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeAuto},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: oldInput.Name, Input: oldInput, Records: oldRecords},
		ensureBusFn: func() error {
			return nil
		},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			return staleErr
		},
	}
	publisher.updateServiceTXTFn = func(context.Context, ServiceRecord) error {
		return staleErr
	}

	status, publishErr := publisher.Update(context.Background(), ServiceInput{
		Name:     oldInput.Name,
		Hostname: oldInput.Hostname,
		Port:     oldInput.Port,
		IPPS:     true,
		TXT:      map[string]string{"note": "new"},
	})
	if publishErr != nil {
		t.Fatalf("Update returned transport error: %v", publishErr)
	}
	if status.State != StateDegraded {
		t.Fatalf("Update state = %v, want degraded", status.State)
	}
	if publisher.group != "" || publisher.current != nil {
		t.Fatalf("stale entry group state was retained after UnknownObject: group=%q current=%#v", publisher.group, publisher.current)
	}
}

func TestAvahiRetryRebuildsAfterInvalidEntryGroup(t *testing.T) {
	oldInput := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	oldRecords, err := BuildRecords(oldInput)
	if err != nil {
		t.Fatalf("BuildRecords(old): %v", err)
	}

	staleErr := errors.New("org.freedesktop.DBus.Error.UnknownObject: entry group no longer exists")
	createCalls := 0
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeAuto},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: oldInput.Name, Input: oldInput, Records: oldRecords},
		ensureBusFn: func() error {
			return nil
		},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			return staleErr
		},
		createGroupFn: func(_ context.Context, input ServiceInput, records []ServiceRecord) (dbus.ObjectPath, bool, error) {
			createCalls++
			if input.TXT["note"] != "new" || len(records) == 0 {
				return "", false, fmt.Errorf("retry used the wrong publication snapshot: input=%#v records=%#v", input, records)
			}
			return dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup2"), false, nil
		},
	}
	publisher.updateServiceTXTFn = func(context.Context, ServiceRecord) error {
		return staleErr
	}

	status, publishErr := publisher.Update(context.Background(), ServiceInput{
		Name:     oldInput.Name,
		Hostname: oldInput.Hostname,
		Port:     oldInput.Port,
		IPPS:     true,
		TXT:      map[string]string{"note": "new"},
	})
	if publishErr != nil {
		t.Fatalf("first Update returned transport error: %v", publishErr)
	}
	if status.State != StateDegraded {
		t.Fatalf("first Update state = %v, want degraded", status.State)
	}

	publisher.updateServiceTXTFn = nil
	status, publishErr = publisher.Update(context.Background(), ServiceInput{
		Name:     oldInput.Name,
		Hostname: oldInput.Hostname,
		Port:     oldInput.Port,
		IPPS:     true,
		TXT:      map[string]string{"note": "new"},
	})
	if publishErr != nil {
		t.Fatalf("retry Update returned transport error: %v", publishErr)
	}
	if status.State != StatePublished {
		t.Fatalf("retry Update state = %v, want published", status.State)
	}
	if createCalls != 1 {
		t.Fatalf("retry createGroup calls = %d, want 1", createCalls)
	}
	if publisher.group != dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup2") || publisher.current == nil {
		t.Fatalf("retry did not retain rebuilt publication: group=%q current=%#v", publisher.group, publisher.current)
	}
}

func TestAvahiDisabledUpdateTreatsGoneEntryGroupAsWithdrawn(t *testing.T) {
	input := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	records, err := BuildRecords(input)
	if err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}

	freeErr := errors.New("org.freedesktop.DBus.Error.UnknownObject: entry group no longer exists")
	freeCalled := false
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeOff},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: input.Name, Input: input, Records: records},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			freeCalled = true
			return freeErr
		},
	}

	status, publishErr := publisher.Update(context.Background(), input)
	if publishErr != nil {
		t.Fatalf("Update returned transport error: %v", publishErr)
	}
	if status.State != StateDisabled {
		t.Fatalf("Update state = %v, want disabled", status.State)
	}
	if !freeCalled {
		t.Fatal("Update did not attempt to free the stale entry group")
	}
	if publisher.group != "" || publisher.current != nil {
		t.Fatalf("disabled update retained stale publication state: group=%q current=%#v", publisher.group, publisher.current)
	}
}

func TestAvahiWithdrawTreatsGoneEntryGroupAsWithdrawn(t *testing.T) {
	input := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	records, err := BuildRecords(input)
	if err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}

	freeErr := errors.New("org.freedesktop.DBus.Error.ServiceUnknown: Avahi disappeared")
	freeCalled := false
	publisher := &avahiPublisher{
		cfg:     config.DNSSDConfig{Mode: config.DNSModeAuto},
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: input.Name, Input: input, Records: records},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			freeCalled = true
			return freeErr
		},
	}

	status, publishErr := publisher.Withdraw(context.Background())
	if publishErr != nil {
		t.Fatalf("Withdraw returned transport error: %v", publishErr)
	}
	if status.State != StateWithdrawn {
		t.Fatalf("Withdraw state = %v, want withdrawn", status.State)
	}
	if !freeCalled {
		t.Fatal("Withdraw did not attempt to free the stale entry group")
	}
	if publisher.group != "" || publisher.current != nil {
		t.Fatalf("withdraw retained stale publication state: group=%q current=%#v", publisher.group, publisher.current)
	}
}

func TestAvahiCloseTreatsGoneEntryGroupAsClosed(t *testing.T) {
	input := ServiceInput{
		Name:     "Office",
		Hostname: "printer.local",
		Port:     631,
		IPPS:     true,
		TXT:      map[string]string{"note": "old"},
	}
	records, err := BuildRecords(input)
	if err != nil {
		t.Fatalf("BuildRecords: %v", err)
	}

	freeErr := errors.New("org.freedesktop.DBus.Error.UnknownObject: entry group no longer exists")
	freeCalled := false
	publisher := &avahiPublisher{
		group:   dbus.ObjectPath("/org/freedesktop/Avahi/EntryGroup1"),
		current: &Publication{Name: input.Name, Input: input, Records: records},
		freeGroupFn: func(context.Context, dbus.ObjectPath) error {
			freeCalled = true
			return freeErr
		},
	}

	if err := publisher.Close(); err != nil {
		t.Fatalf("Close returned an error for an already-gone group: %v", err)
	}
	if !freeCalled {
		t.Fatal("Close did not attempt to free the stale entry group")
	}
	if !publisher.closed {
		t.Fatal("Close did not mark the publisher closed")
	}
	if publisher.group != "" || publisher.current != nil {
		t.Fatalf("Close retained stale publication state: group=%q current=%#v", publisher.group, publisher.current)
	}
}
