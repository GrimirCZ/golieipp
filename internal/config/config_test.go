package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
