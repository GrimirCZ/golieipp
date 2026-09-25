package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const statisticsTestBase = "listen:\n  public_base_url: ipp://proxy/printers\nprinters:\n  office:\n    upstream_uri: ipp://printer/ipp/print\n"

func TestStatisticsDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name, section string
		check         func(*testing.T, StatisticsConfig)
	}{
		{"defaults", "", func(t *testing.T, s StatisticsConfig) {
			if !s.Enabled || !s.Resources || !s.CPU || s.SQLitePath != "jobs.db.stats.sqlite" || s.DetailRetention != 90*24*time.Hour || s.CPURetention != 30*24*time.Hour || s.RollupRetention != 0 || s.QueueSize != 4096 || s.BatchSize != 256 || s.FlushInterval != time.Second {
				t.Fatalf("defaults: %+v", s)
			}
		}},
		{"disabled", "statistics:\n  enabled: false\n", func(t *testing.T, s StatisticsConfig) {
			if s.Enabled {
				t.Fatal("enabled")
			}
		}},
		{"overrides", "statistics:\n  detail_retention: 0\n  cpu_retention: 2d\n  rollup_retention: 365d\n  resources: false\n  cpu: false\n  sqlite_path: usage.sqlite\n  queue_size: 10\n  batch_size: 3\n  flush_interval: 500ms\n", func(t *testing.T, s StatisticsConfig) {
			if s.DetailRetention != 0 || s.CPURetention != 48*time.Hour || s.RollupRetention != 365*24*time.Hour || s.Resources || s.CPU || s.SQLitePath != "usage.sqlite" || s.QueueSize != 10 || s.BatchSize != 3 || s.FlushInterval != 500*time.Millisecond {
				t.Fatalf("overrides: %+v", s)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfigYAML(t, statisticsTestBase+tc.section)
			tc.check(t, cfg.Statistics)
		})
	}
}

func TestStatisticsInvalidConfiguration(t *testing.T) {
	for _, section := range []string{"queue_size: 0", "batch_size: 5000", "flush_interval: 0", "detail_retention: -1d", "cpu_retention: -1h", "rollup_retention: -1d", "sqlite_path: jobs.db"} {
		t.Run(section, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(statisticsTestBase+"statistics:\n  "+section+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), "statistics") {
				t.Fatalf("expected statistics error, got %v", err)
			}
		})
	}
}
