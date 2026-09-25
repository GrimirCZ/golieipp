package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// StatisticsConfig is independent of registry retention. Zero retention keeps
// data indefinitely; disabled collection neither opens a DB nor starts workers.
type StatisticsConfig struct {
	Enabled         bool          `yaml:"enabled"`
	SQLitePath      string        `yaml:"sqlite_path"`
	DetailRetention time.Duration `yaml:"detail_retention"`
	CPURetention    time.Duration `yaml:"cpu_retention"`
	RollupRetention time.Duration `yaml:"rollup_retention"`
	QueueSize       int           `yaml:"queue_size"`
	BatchSize       int           `yaml:"batch_size"`
	FlushInterval   time.Duration `yaml:"flush_interval"`
	Resources       bool          `yaml:"resources"`
	CPU             bool          `yaml:"cpu"`
	configured      bool
}

func DefaultStatisticsConfig() StatisticsConfig {
	return StatisticsConfig{Enabled: true, DetailRetention: 90 * 24 * time.Hour,
		CPURetention: 30 * 24 * time.Hour, QueueSize: 4096, BatchSize: 256,
		FlushInterval: time.Second, Resources: true, CPU: true, configured: true}
}

func (s *StatisticsConfig) UnmarshalYAML(node *yaml.Node) error {
	*s = DefaultStatisticsConfig()
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("statistics must be a YAML mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		var err error
		switch key {
		case "enabled":
			err = value.Decode(&s.Enabled)
		case "sqlite_path":
			err = value.Decode(&s.SQLitePath)
		case "resources":
			err = value.Decode(&s.Resources)
		case "cpu":
			err = value.Decode(&s.CPU)
		case "queue_size":
			err = value.Decode(&s.QueueSize)
		case "batch_size":
			err = value.Decode(&s.BatchSize)
		case "detail_retention":
			s.DetailRetention, err = parseDurationNode(value)
		case "cpu_retention":
			s.CPURetention, err = parseDurationNode(value)
		case "rollup_retention":
			s.RollupRetention, err = parseDurationNode(value)
		case "flush_interval":
			s.FlushInterval, err = parseDurationNode(value)
		}
		if err != nil {
			return fmt.Errorf("statistics.%s: %w", key, err)
		}
	}
	return nil
}

func (s *StatisticsConfig) applyDefaults(primary string) {
	if !s.configured && *s == (StatisticsConfig{}) {
		*s = DefaultStatisticsConfig()
	}
	if s.SQLitePath == "" {
		s.SQLitePath = primary + ".stats.sqlite"
	}
}

func (s StatisticsConfig) validate(primary string) error {
	// Programmatically constructed configurations may leave the section zero;
	// normal file loading supplies all defaults before validation.
	if !s.Enabled {
		return nil
	}
	if s.QueueSize < 1 || s.QueueSize > 1_000_000 {
		return fmt.Errorf("queue_size must be between 1 and 1000000")
	}
	if s.BatchSize < 1 || s.BatchSize > s.QueueSize {
		return fmt.Errorf("batch_size must be positive and no greater than queue_size")
	}
	if s.FlushInterval <= 0 {
		return fmt.Errorf("flush_interval must be positive")
	}
	if s.DetailRetention < 0 || s.CPURetention < 0 || s.RollupRetention < 0 {
		return fmt.Errorf("retention periods must be nonnegative (0 means indefinitely)")
	}
	if s.SQLitePath == "" {
		return fmt.Errorf("sqlite_path must not be empty")
	}
	a, err := filepath.Abs(primary)
	if err != nil {
		return err
	}
	b, err := filepath.Abs(s.SQLitePath)
	if err != nil {
		return err
	}
	if a == b {
		return fmt.Errorf("sqlite_path must differ from storage.sqlite_path")
	}
	pa, ea := os.Stat(a)
	pb, eb := os.Stat(b)
	if ea == nil && eb == nil && os.SameFile(pa, pb) {
		return fmt.Errorf("sqlite_path must differ from storage.sqlite_path")
	}
	return nil
}
