package config

import (
	"os"
	"path/filepath"
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
