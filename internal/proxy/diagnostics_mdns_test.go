//go:build linux && avahi

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/dnssd"
)

func TestReadinessIncludesMinimalMDNSSummaryWhenCompiledIn(t *testing.T) {
	cfg := &config.Config{
		DNSSD: config.DNSSDConfig{Mode: config.DNSModeAuto},
		Printers: map[string]config.PrinterConfig{
			"office": {DNSSD: true},
		},
	}
	svc, err := NewService(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	svc.mu.Lock()
	svc.queueHealth["office"] = queueHealth{DNSState: string(dnssd.StatePublished)}
	svc.mu.Unlock()

	recorder := httptest.NewRecorder()
	svc.Routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("readyz returned %d", recorder.Code)
	}
	var readiness readinessResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &readiness); err != nil {
		t.Fatal(err)
	}
	if readiness.MDNS == nil || readiness.MDNS.State != string(dnssd.StatePublished) {
		t.Fatalf("mDNS readiness summary = %+v, want published", readiness.MDNS)
	}
	if strings.Contains(recorder.Body.String(), "_ipp._tcp") {
		t.Fatalf("readyz exposed registration details: %s", recorder.Body.String())
	}
}

func TestDumpStateIncludesCompleteMDNSRegistrationsAndLastError(t *testing.T) {
	var logBuffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := &config.Config{
		DNSSD: config.DNSSDConfig{Mode: config.DNSModeAuto, Hostname: "proxy.local", Interface: "eth0"},
		Printers: map[string]config.PrinterConfig{
			"office": {DNSSD: true},
		},
	}
	svc, err := NewService(cfg, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	publisher := dnssd.NewStubPublisher()
	svc.mu.Lock()
	svc.publishers["office"] = publisher
	svc.mu.Unlock()

	input := dnssd.ServiceInput{
		Name:     "Office",
		Hostname: "proxy.local",
		Port:     8631,
		TXT: map[string]string{
			"rp":  "printers/office",
			"pdl": "application/pdf",
		},
	}
	status, err := publisher.Publish(context.Background(), input)
	if err != nil || status.State != dnssd.StatePublished {
		t.Fatalf("initial publish status=%+v err=%v", status, err)
	}
	svc.mu.Lock()
	health := svc.queueHealth["office"]
	health.IPPEligible = true
	svc.queueHealth["office"] = health
	svc.mu.Unlock()
	svc.recordDNSStatus("office", status)

	publisher.SetAvailable(false)
	status, err = publisher.Update(context.Background(), input)
	if err != nil || status.State != dnssd.StateDegraded {
		t.Fatalf("degraded update status=%+v err=%v", status, err)
	}
	svc.recordDNSStatus("office", status)
	svc.DumpState()

	output := logBuffer.String()
	for _, want := range []string{
		`"section":"mdns"`,
		`"publisher_backend":"stub"`,
		`"publisher_availability":"unavailable"`,
		`"present":true`,
		`"last_error":"DNS-SD publisher unavailable"`,
		`"full_type":"_universal._sub._ipp._tcp"`,
		`"rp":"printers/office"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("state dump missing %q: %s", want, output)
		}
	}
}
