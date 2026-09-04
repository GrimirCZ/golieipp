package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
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
	// Media is the legacy single-value media policy. It remains part of the
	// in-memory configuration so callers that construct PolicyConfig values can
	// continue to use the old API. YAML configurations should use
	// media_supported and media_default instead.
	Media          string   `yaml:"media"`
	MediaSupported []string `yaml:"media_supported"`
	MediaDefault   string   `yaml:"media_default"`
	MediaType      string   `yaml:"media_type"`
	PrintColorMode string   `yaml:"print_color_mode"`
	MediaSource    *string  `yaml:"media_source"`
	PrintScaling   *string  `yaml:"print_scaling"`
	UseMediaCol    bool     `yaml:"use_media_col"`

	// The presence bits let validation distinguish an omitted YAML key from an
	// explicitly empty one. They are intentionally not exported or serialized.
	mediaLegacySet     bool
	mediaSupportedSet  bool
	mediaDefaultSet    bool
	mediaDefaultsApply bool
}

// UnmarshalYAML records which media policy keys were present. yaml.v3 does
// not otherwise distinguish a missing scalar from an explicitly empty scalar,
// which matters because the new list/default form requires both keys.
func (p *PolicyConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain PolicyConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*p = PolicyConfig(decoded)
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("policy must be a YAML mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "media":
			p.mediaLegacySet = true
		case "media_supported":
			p.mediaSupportedSet = true
		case "media_default":
			p.mediaDefaultSet = true
		}
	}
	return nil
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
		printer.Policy.applyMediaDefaults()
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

func (p *PolicyConfig) applyMediaDefaults() {
	// A valid legacy scalar is promoted to the normalized list/default form.
	// Leave mixed or incomplete new configurations untouched so Validate can
	// return a useful error instead of silently repairing them.
	legacy := p.mediaLegacySet || p.Media != ""
	newForm := p.mediaSupportedSet || p.mediaDefaultSet || len(p.MediaSupported) > 0 || p.MediaDefault != ""
	if legacy && !newForm {
		p.Media = strings.TrimSpace(p.Media)
		p.MediaSupported = []string{p.Media}
		p.MediaDefault = p.Media
		p.mediaDefaultsApply = true
		return
	}
	if !legacy && !newForm {
		const defaultMedia = "iso_a4_210x297mm"
		p.Media = defaultMedia
		p.MediaSupported = []string{defaultMedia}
		p.MediaDefault = defaultMedia
		p.mediaDefaultsApply = true
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
		if err := validatePolicyMedia(printer.Policy); err != nil {
			return fmt.Errorf("printers.%s.policy: %w", name, err)
		}
		if printer.Policy.PrintColorMode == "" {
			return fmt.Errorf("printers.%s.policy.print_color_mode is required", name)
		}
	}
	return nil
}

func validatePolicyMedia(p PolicyConfig) error {
	legacy := p.mediaLegacySet || p.Media != ""
	newForm := p.mediaSupportedSet || p.mediaDefaultSet || len(p.MediaSupported) > 0 || p.MediaDefault != ""
	if legacy && newForm && !p.mediaDefaultsApply {
		return errors.New("media cannot be combined with media_supported or media_default")
	}
	if p.mediaDefaultsApply {
		newForm = true
		legacy = false
	}
	if newForm {
		if len(p.MediaSupported) == 0 {
			return errors.New("media_supported must contain at least one value")
		}
		if strings.TrimSpace(p.MediaDefault) == "" {
			return errors.New("media_default is required when media_supported is configured")
		}
		seen := make(map[string]struct{}, len(p.MediaSupported))
		for i, raw := range p.MediaSupported {
			value := strings.TrimSpace(raw)
			if value == "" {
				return fmt.Errorf("media_supported[%d] must not be empty", i)
			}
			key := strings.ToLower(value)
			if _, ok := seen[key]; ok {
				return fmt.Errorf("media_supported contains duplicate value %q", raw)
			}
			seen[key] = struct{}{}
		}
		defaultValue := strings.ToLower(strings.TrimSpace(p.MediaDefault))
		if _, ok := seen[defaultValue]; !ok {
			return fmt.Errorf("media_default %q is not in media_supported", p.MediaDefault)
		}
		return nil
	}
	if p.Media == "" {
		return errors.New("media or media_supported is required")
	}
	if strings.TrimSpace(p.Media) == "" {
		return errors.New("media must not be empty")
	}
	return nil
}
