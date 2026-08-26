package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   ListenConfig             `yaml:"listen"`
	Storage  StorageConfig            `yaml:"storage"`
	Printers map[string]PrinterConfig `yaml:"printers"`
}

type ListenConfig struct {
	Addr          string `yaml:"addr"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
}

type PrinterConfig struct {
	UpstreamURI     string            `yaml:"upstream_uri"`
	DisplayName     string            `yaml:"display_name"`
	Location        string            `yaml:"location"`
	Optional        bool              `yaml:"optional"`
	RefreshInterval time.Duration     `yaml:"-"`
	RefreshRaw      string            `yaml:"refresh_interval"`
	Policy          PolicyConfig      `yaml:"policy"`
	Passthrough     PassthroughConfig `yaml:"passthrough"`
}

type PolicyConfig struct {
	Media          string  `yaml:"media"`
	MediaType      string  `yaml:"media_type"`
	PrintColorMode string  `yaml:"print_color_mode"`
	MediaSource    *string `yaml:"media_source"`
	PrintScaling   *string `yaml:"print_scaling"`
	UseMediaCol    bool    `yaml:"use_media_col"`
}

type PassthroughConfig struct {
	AllowUnknownAttributes bool     `yaml:"allow_unknown_attributes"`
	PreserveJobAttrs       []string `yaml:"preserve_job_attrs"`
	DropVendorAttrs        []string `yaml:"drop_vendor_attrs"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen.Addr == "" {
		c.Listen.Addr = ":8631"
	}
	if c.Storage.SQLitePath == "" {
		c.Storage.SQLitePath = "jobs.db"
	}
	for name, printer := range c.Printers {
		if printer.DisplayName == "" {
			printer.DisplayName = name
		}
		if printer.Policy.Media == "" {
			printer.Policy.Media = "iso_a4_210x297mm"
		}
		if printer.Policy.MediaType == "" {
			printer.Policy.MediaType = "stationery"
		}
		if printer.Policy.PrintColorMode == "" {
			printer.Policy.PrintColorMode = "monochrome"
		}
		if printer.RefreshRaw == "" {
			printer.RefreshInterval = 5 * time.Minute
		} else if d, err := time.ParseDuration(printer.RefreshRaw); err == nil {
			printer.RefreshInterval = d
		}
		if len(printer.Passthrough.DropVendorAttrs) == 0 {
			printer.Passthrough.DropVendorAttrs = []string{
				"ColorModel", "ColorMode", "InputSlot", "PageSize", "MediaType",
				"HPPaperSource", "BRMediaType", "RIPaperPolicy",
			}
		}
		c.Printers[name] = printer
	}
}

func (c *Config) Validate() error {
	if c.Listen.PublicBaseURL == "" {
		return errors.New("listen.public_base_url is required")
	}
	if len(c.Printers) == 0 {
		return errors.New("at least one printer is required")
	}
	for name, printer := range c.Printers {
		if printer.UpstreamURI == "" {
			return fmt.Errorf("printers.%s.upstream_uri is required", name)
		}
		if printer.RefreshRaw != "" && printer.RefreshInterval == 0 {
			return fmt.Errorf("printers.%s.refresh_interval is invalid", name)
		}
		if printer.Policy.Media == "" || printer.Policy.PrintColorMode == "" {
			return fmt.Errorf("printers.%s.policy media and print_color_mode are required", name)
		}
	}
	return nil
}
