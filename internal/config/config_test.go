package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOptionalPrinter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
listen:
  public_base_url: "ipp://proxy/printers"
printers:
  office:
    upstream_uri: "ipp://printer/ipp/print"
    optional: true
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Printers["office"].Optional {
		t.Fatal("expected printer to be optional")
	}
}

func TestLoadLegacyMediaPromotesToSupportedAndDefault(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy/printers"
printers:
  office:
    upstream_uri: "ipp://printer/ipp/print"
    policy:
      media: "na_letter_8.5x11in"
`)
	policy := cfg.Printers["office"].Policy
	if policy.Media != "na_letter_8.5x11in" || policy.MediaDefault != "na_letter_8.5x11in" {
		t.Fatalf("legacy media was not promoted: %+v", policy)
	}
	if len(policy.MediaSupported) != 1 || policy.MediaSupported[0] != policy.Media {
		t.Fatalf("legacy supported list was not promoted: %+v", policy.MediaSupported)
	}
}

func TestLoadNewMediaPolicy(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy/printers"
printers:
  office:
    upstream_uri: "ipp://printer/ipp/print"
    policy:
      media_supported: ["iso_a4_210x297mm", "na_letter_8.5x11in"]
      media_default: "iso_a4_210x297mm"
`)
	policy := cfg.Printers["office"].Policy
	if got := strings.Join(policy.MediaSupported, ","); got != "iso_a4_210x297mm,na_letter_8.5x11in" {
		t.Fatalf("unexpected media list %q", got)
	}
	if policy.MediaDefault != "iso_a4_210x297mm" || policy.Media != "" {
		t.Fatalf("unexpected new media policy: %+v", policy)
	}
}

func TestLoadMediaPolicyValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "mixed",
			body: `media: "iso_a4_210x297mm"
media_supported: ["iso_a4_210x297mm"]
media_default: "iso_a4_210x297mm"`,
			want: "cannot be combined",
		},
		{
			name: "missing default",
			body: `media_supported: ["iso_a4_210x297mm"]`,
			want: "media_default is required",
		},
		{
			name: "missing list",
			body: `media_default: "iso_a4_210x297mm"`,
			want: "media_supported must contain",
		},
		{
			name: "empty entry",
			body: "media_supported: [\"iso_a4_210x297mm\", \"\"]\nmedia_default: \"iso_a4_210x297mm\"",
			want: "must not be empty",
		},
		{
			name: "duplicate",
			body: "media_supported: [\"iso_a4_210x297mm\", \"ISO_A4_210X297MM\"]\nmedia_default: \"iso_a4_210x297mm\"",
			want: "duplicate",
		},
		{
			name: "default outside list",
			body: "media_supported: [\"iso_a4_210x297mm\"]\nmedia_default: \"na_letter_8.5x11in\"",
			want: "not in media_supported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := fmt.Sprintf(`
listen:
  public_base_url: "ipp://proxy/printers"
printers:
  office:
    upstream_uri: "ipp://printer/ipp/print"
    policy:
      %s
`, strings.ReplaceAll(test.body, "\n", "\n      "))
			_, err := loadConfigYAMLError(t, body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestLoadWithoutMediaKeepsA4Default(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy/printers"
printers:
  office:
    upstream_uri: "ipp://printer/ipp/print"
`)
	policy := cfg.Printers["office"].Policy
	if policy.Media != "iso_a4_210x297mm" || policy.MediaDefault != policy.Media || len(policy.MediaSupported) != 1 {
		t.Fatalf("unexpected implicit A4 policy: %+v", policy)
	}
}

func TestLoadAppliesDNSDefaultsAndPreservesExplicitFalse(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  default:
    upstream_uri: "ipp://printer.example/ipp/print"
  disabled:
    upstream_uri: "ipps://secure-printer.example/ipp/print"
    ipp_everywhere_mode: disabled
    dns_sd: false
    geo_location: "geo:50.0755,14.4378"
`)
	if cfg.DNSSD.Mode != DNSModeAuto {
		t.Fatalf("expected DNS-SD mode %q, got %q", DNSModeAuto, cfg.DNSSD.Mode)
	}
	if got := cfg.Printers["default"].IPPEverywhereMode; got != IPPEverywhereAuto {
		t.Fatalf("expected IPP Everywhere default %q, got %q", IPPEverywhereAuto, got)
	}
	if !cfg.Printers["default"].DNSSD {
		t.Fatal("expected printer DNS-SD to default to enabled")
	}
	if cfg.Printers["disabled"].DNSSD {
		t.Fatal("explicit dns_sd: false was not preserved")
	}
	if cfg.Printers["disabled"].GeoLocation != "geo:50.0755,14.4378" {
		t.Fatalf("unexpected geo location %q", cfg.Printers["disabled"].GeoLocation)
	}
	if got := cfg.Printers["default"].AirPrintMode; got != AirPrintAuto {
		t.Fatalf("expected AirPrint default %q, got %q", AirPrintAuto, got)
	}
}

func TestLoadDNSAllowedAliases(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://public.example:8631/printers"
dns_sd:
  allowed_aliases:
    - dns-sd.example
    - 198.51.100.10
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	got := cfg.DNSSD.AllowedAliases
	if len(got) != 2 || got[0] != "dns-sd.example" || got[1] != "198.51.100.10" {
		t.Fatalf("unexpected DNS-SD allowed aliases: %#v", got)
	}
}

func TestLoadRejectsInvalidDNSAllowedAlias(t *testing.T) {
	_, err := loadConfigYAMLError(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
dns_sd:
  allowed_aliases:
    - "http://dns-sd.example"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	if err == nil || !strings.Contains(err.Error(), "allowed_aliases") {
		t.Fatalf("expected invalid DNS-SD alias error, got %v", err)
	}
}

func TestLoadNormalizesLegacyIPPEverywhereRequiredWithWarning(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
    ipp_everywhere_mode: required
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithLogger(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Printers["office"].IPPEverywhereMode; got != IPPEverywhereAuto {
		t.Fatalf("legacy mode was not normalized: %q", got)
	}
	if !strings.Contains(logs.String(), "ipp_everywhere_mode=required is deprecated") {
		t.Fatalf("migration warning missing: %s", logs.String())
	}
}

func TestLoadWarnsOnUnknownAndDeprecatedYAMLKeys(t *testing.T) {
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn}))
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`
listen:
  public_base_url: "ipp://proxy.example/printers"
  future_listen_key: true
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
    future_printer_key: true
    passthrough:
      allow_unknown_attributes: true
      future_passthrough_key: true
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithLogger(path, logger); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"listen.future_listen_key",
		"printers.office.future_printer_key",
		"printers.office.passthrough.future_passthrough_key",
		"passthrough.allow_unknown_attributes",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("warning log did not contain %q:\n%s", want, log.String())
		}
	}
}

func TestLoadAppliesGlobalResourceDefaults(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	if got, want := cfg.Defaults.MaxEnvelopeBytes, int64(1<<20); got != want {
		t.Fatalf("envelope default = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxDocumentBytes, int64(1<<30); got != want {
		t.Fatalf("document default = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxUpstreamResponseBytes, int64(32<<20); got != want {
		t.Fatalf("upstream response default = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxConcurrentPayloadJobsPerQueue, 2; got != want {
		t.Fatalf("concurrency default = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.JobRetention, 30*24*time.Hour; got != want {
		t.Fatalf("retention default = %s, want %s", got, want)
	}
}

func TestLoadParsesGlobalResourceLimits(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
defaults:
  max_envelope_bytes: 2MiB
  max_document_bytes: "3GiB"
  max_upstream_response_bytes: 4MiB
  max_concurrent_payload_jobs_per_queue: 7
  job_retention: 48h
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	if got, want := cfg.Defaults.MaxEnvelopeBytes, int64(2<<20); got != want {
		t.Fatalf("envelope = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxDocumentBytes, int64(3<<30); got != want {
		t.Fatalf("document = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxUpstreamResponseBytes, int64(4<<20); got != want {
		t.Fatalf("upstream response = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.MaxConcurrentPayloadJobsPerQueue, 7; got != want {
		t.Fatalf("concurrency = %d, want %d", got, want)
	}
	if got, want := cfg.Defaults.JobRetention, 48*time.Hour; got != want {
		t.Fatalf("retention = %s, want %s", got, want)
	}
	if cfg.Limits != cfg.Defaults {
		t.Fatalf("limits alias was not synchronized: limits=%+v defaults=%+v", cfg.Limits, cfg.Defaults)
	}
}

func TestLoadRejectsInvalidURIsEnumsKeywordsAndRanges(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "public URI", body: `listen:
  public_base_url: "http://proxy.example/printers"`, want: "listen.public_base_url"},
		{name: "public IPPS URI", body: `listen:
  public_base_url: "ipps://proxy.example/printers"`, want: "listen.public_base_url"},
		{name: "public path", body: `listen:
  public_base_url: "ipp://proxy.example/queues"`, want: "listen.public_base_url"},
		{name: "upstream URI", body: `listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://"`, want: "upstream_uri"},
		{name: "IPP Everywhere mode", body: `listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
    ipp_everywhere_mode: sometimes`, want: "ipp_everywhere_mode"},
		{name: "keyword", body: `listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
    policy:
      media: "not a keyword"`, want: "media"},
		{name: "refresh range", body: `listen:
  public_base_url: "ipp://proxy.example/printers"
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
    refresh_interval: "-1m"`, want: "refresh_interval"},
		{name: "default range", body: `listen:
  public_base_url: "ipp://proxy.example/printers"
defaults:
  max_document_bytes: 0
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"`, want: "max_document_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadConfigYAMLError(t, test.body)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestLoadRejectsNonPositiveRetentionAndSupportsDayDurations(t *testing.T) {
	cfg := loadConfigYAML(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
defaults:
  retention: 2d
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	if got, want := cfg.Defaults.JobRetention, 48*time.Hour; got != want {
		t.Fatalf("retention = %s, want %s", got, want)
	}
	_, err := loadConfigYAMLError(t, `
listen:
  public_base_url: "ipp://proxy.example/printers"
defaults:
  retention: 0s
printers:
  office:
    upstream_uri: "ipp://printer.example/ipp/print"
`)
	if err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("expected non-positive retention error, got %v", err)
	}
}

func loadConfigYAML(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := loadConfigYAMLError(t, body)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func loadConfigYAMLError(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}
