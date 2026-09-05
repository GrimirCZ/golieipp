package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen  ListenConfig  `yaml:"listen"`
	Storage StorageConfig `yaml:"storage"`
	// Defaults contains process-wide resource limits. Limits is retained as a
	// spelling-compatible alias for deployments that prefer that section name;
	// after loading both fields contain the same effective values.
	Defaults DefaultsConfig           `yaml:"defaults"`
	Limits   DefaultsConfig           `yaml:"limits"`
	DNSSD    DNSSDConfig              `yaml:"dns_sd"`
	Printers map[string]PrinterConfig `yaml:"printers"`

	limitsAliasConflict bool
}

type ListenConfig struct {
	Addr          string `yaml:"addr"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type StorageConfig struct {
	SQLitePath string `yaml:"sqlite_path"`
}

const (
	DNSModeAuto = "auto"
	DNSModeOff  = "off"

	// DNSSDModeAuto and DNSSDModeOff are descriptive aliases for callers that
	// use the DNS-SD acronym in their own configuration code.
	DNSSDModeAuto = DNSModeAuto
	DNSSDModeOff  = DNSModeOff

	IPPEverywhereDisabled = "disabled"
	IPPEverywhereAuto     = "auto"
	IPPEverywhereRequired = "required"

	IPPEverywhereModeDisabled = IPPEverywhereDisabled
	IPPEverywhereModeAuto     = IPPEverywhereAuto
	IPPEverywhereModeRequired = IPPEverywhereRequired
)

type DNSSDConfig struct {
	Mode      string `yaml:"mode"`
	Hostname  string `yaml:"hostname"`
	Interface string `yaml:"interface"`

	modeSet bool
}

// UnmarshalYAML records whether mode was present so an explicitly empty mode
// is rejected while an omitted mode can receive the documented default.
func (d *DNSSDConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*d = DNSSDConfig{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("dns_sd must be a YAML mapping")
	}
	type plain DNSSDConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*d = DNSSDConfig(decoded)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "mode" {
			d.modeSet = true
		}
	}
	return nil
}

// DefaultsConfig contains process-wide resource and retention defaults. Byte
// limits accept either a byte count or a human-readable IEC value such as
// "1MiB". Duration values accept Go duration syntax and a convenient "d"
// suffix for days.
type DefaultsConfig struct {
	MaxEnvelopeBytes                 int64         `yaml:"max_envelope_bytes"`
	MaxDocumentBytes                 int64         `yaml:"max_document_bytes"`
	MaxUpstreamResponseBytes         int64         `yaml:"max_upstream_response_bytes"`
	MaxConcurrentPayloadJobsPerQueue int           `yaml:"max_concurrent_payload_jobs_per_queue"`
	JobRetention                     time.Duration `yaml:"job_retention"`

	// These aliases make the in-memory API readable from either noun-first or
	// limit-first call sites. They are synchronized with the canonical fields by
	// Config.applyDefaults and are not additional YAML keys.
	EnvelopeMaxBytes              int64         `yaml:"-"`
	DocumentMaxBytes              int64         `yaml:"-"`
	UpstreamResponseMaxBytes      int64         `yaml:"-"`
	ConcurrentPayloadJobsPerQueue int           `yaml:"-"`
	Retention                     time.Duration `yaml:"-"`

	maxEnvelopeSet         bool
	maxDocumentSet         bool
	maxUpstreamResponseSet bool
	maxConcurrentSet       bool
	jobRetentionSet        bool
}

// LimitsConfig is an alias for callers that name the section after its
// purpose rather than its defaulting behavior.
type LimitsConfig = DefaultsConfig

func (d *DefaultsConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*d = DefaultsConfig{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("defaults must be a YAML mapping")
	}
	*d = DefaultsConfig{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		switch key {
		case "max_envelope_bytes", "envelope", "envelope_max_bytes":
			parsed, err := parseByteLimitNode(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			d.MaxEnvelopeBytes = parsed
			d.maxEnvelopeSet = true
		case "max_document_bytes", "document", "document_max_bytes":
			parsed, err := parseByteLimitNode(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			d.MaxDocumentBytes = parsed
			d.maxDocumentSet = true
		case "max_upstream_response_bytes", "upstream_response", "upstream_response_max_bytes":
			parsed, err := parseByteLimitNode(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			d.MaxUpstreamResponseBytes = parsed
			d.maxUpstreamResponseSet = true
		case "max_concurrent_payload_jobs_per_queue", "concurrent_payload_jobs_per_queue":
			parsed, err := parsePositiveIntNode(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			d.MaxConcurrentPayloadJobsPerQueue = parsed
			d.maxConcurrentSet = true
		case "job_retention", "retention":
			parsed, err := parseDurationNode(value)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			d.JobRetention = parsed
			d.jobRetentionSet = true
		}
	}
	return nil
}

type PrinterConfig struct {
	UpstreamURI       string            `yaml:"upstream_uri"`
	DisplayName       string            `yaml:"display_name"`
	Location          string            `yaml:"location"`
	Optional          bool              `yaml:"optional"`
	IPPEverywhereMode string            `yaml:"ipp_everywhere_mode"`
	DNSSD             bool              `yaml:"dns_sd"`
	GeoLocation       string            `yaml:"geo_location"`
	RefreshInterval   time.Duration     `yaml:"-"`
	RefreshRaw        string            `yaml:"refresh_interval"`
	Policy            PolicyConfig      `yaml:"policy"`
	Passthrough       PassthroughConfig `yaml:"passthrough"`

	// DNS_SD is an API spelling alias for DNSSD. YAML uses dns_sd and the
	// canonical in-memory field is DNSSD, matching the top-level section.
	DNS_SD bool `yaml:"-"`

	ippEverywhereModeSet bool
	dnsSDSet             bool
}

// UnmarshalYAML records presence bits for fields whose defaults differ from
// their Go zero values (notably dns_sd, whose default is true).
func (p *PrinterConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		*p = PrinterConfig{}
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("printer must be a YAML mapping")
	}
	type plain PrinterConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*p = PrinterConfig(decoded)
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "ipp_everywhere_mode":
			p.ippEverywhereModeSet = true
		case "dns_sd":
			p.dnsSDSet = true
		}
	}
	return nil
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
	FidelityMode   string   `yaml:"fidelity_mode"`

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
	return LoadWithLogger(path, slog.Default())
}

// LoadWithLogger loads and validates a configuration while reporting unknown
// and deprecated keys through logger. Unknown keys are intentionally warnings
// rather than errors so a newer config can be used with an older binary.
func LoadWithLogger(path string, logger *slog.Logger) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if len(document.Content) > 0 {
		warnUnknownKeys(document.Content[0], logger)
	}
	var cfg Config
	if err := document.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	defaultsProvided := c.Defaults.hasValues()
	limitsProvided := c.Limits.hasValues()
	if defaultsProvided && limitsProvided {
		c.limitsAliasConflict = true
	} else if limitsProvided {
		c.Defaults = c.Limits
	}
	c.Defaults.applyDefaults()
	// Keep both section spellings synchronized for consumers. This also makes
	// the alias useful when a config was built programmatically.
	c.Limits = c.Defaults

	if c.DNSSD.Mode == "" && !c.DNSSD.modeSet {
		c.DNSSD.Mode = DNSModeAuto
	}
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
		if printer.IPPEverywhereMode == "" && !printer.ippEverywhereModeSet {
			printer.IPPEverywhereMode = IPPEverywhereAuto
		}
		if printer.dnsSDSet {
			// Explicit YAML values, including false, are authoritative.
		} else if printer.DNS_SD {
			// Accept the noun-separated Go API alias for callers constructing
			// PrinterConfig values programmatically.
			printer.DNSSD = true
		} else if !printer.DNSSD {
			printer.DNSSD = true
		}
		printer.DNS_SD = printer.DNSSD
		printer.Policy.applyMediaDefaults()
		if printer.Policy.MediaType == "" {
			printer.Policy.MediaType = "stationery"
		}
		if printer.Policy.PrintColorMode == "" {
			printer.Policy.PrintColorMode = "monochrome"
		}
		if printer.Policy.FidelityMode == "" {
			printer.Policy.FidelityMode = "warn"
		}
		if printer.RefreshRaw == "" {
			printer.RefreshInterval = 5 * time.Minute
		} else if d, err := parseFlexibleDuration(printer.RefreshRaw); err == nil {
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

func (d *DefaultsConfig) normalizeAliases() {
	if d.MaxEnvelopeBytes == 0 && d.EnvelopeMaxBytes != 0 {
		d.MaxEnvelopeBytes = d.EnvelopeMaxBytes
	}
	if d.MaxDocumentBytes == 0 && d.DocumentMaxBytes != 0 {
		d.MaxDocumentBytes = d.DocumentMaxBytes
	}
	if d.MaxUpstreamResponseBytes == 0 && d.UpstreamResponseMaxBytes != 0 {
		d.MaxUpstreamResponseBytes = d.UpstreamResponseMaxBytes
	}
	if d.MaxConcurrentPayloadJobsPerQueue == 0 && d.ConcurrentPayloadJobsPerQueue != 0 {
		d.MaxConcurrentPayloadJobsPerQueue = d.ConcurrentPayloadJobsPerQueue
	}
	if d.JobRetention == 0 && d.Retention != 0 {
		d.JobRetention = d.Retention
	}
}

func (d DefaultsConfig) hasValues() bool {
	d.normalizeAliases()
	return d.maxEnvelopeSet || d.maxDocumentSet || d.maxUpstreamResponseSet || d.maxConcurrentSet || d.jobRetentionSet ||
		d.MaxEnvelopeBytes != 0 || d.MaxDocumentBytes != 0 || d.MaxUpstreamResponseBytes != 0 ||
		d.MaxConcurrentPayloadJobsPerQueue != 0 || d.JobRetention != 0
}

func (d *DefaultsConfig) applyDefaults() {
	d.normalizeAliases()
	if d.MaxEnvelopeBytes == 0 && !d.maxEnvelopeSet {
		d.MaxEnvelopeBytes = 1 << 20
	}
	if d.MaxDocumentBytes == 0 && !d.maxDocumentSet {
		d.MaxDocumentBytes = 1 << 30
	}
	if d.MaxUpstreamResponseBytes == 0 && !d.maxUpstreamResponseSet {
		d.MaxUpstreamResponseBytes = 32 << 20
	}
	if d.MaxConcurrentPayloadJobsPerQueue == 0 && !d.maxConcurrentSet {
		d.MaxConcurrentPayloadJobsPerQueue = 2
	}
	if d.JobRetention == 0 && !d.jobRetentionSet {
		d.JobRetention = 30 * 24 * time.Hour
	}
	d.EnvelopeMaxBytes = d.MaxEnvelopeBytes
	d.DocumentMaxBytes = d.MaxDocumentBytes
	d.UpstreamResponseMaxBytes = d.MaxUpstreamResponseBytes
	d.ConcurrentPayloadJobsPerQueue = d.MaxConcurrentPayloadJobsPerQueue
	d.Retention = d.JobRetention
}

func (c *Config) Validate() error {
	if c.Listen.PublicBaseURL == "" {
		return errors.New("listen.public_base_url is required")
	}
	if err := validateIPPURI(c.Listen.PublicBaseURL); err != nil {
		return fmt.Errorf("listen.public_base_url: %w", err)
	}
	if c.limitsAliasConflict {
		return errors.New("defaults and limits cannot both be configured")
	}
	if err := validateDefaults(c.Defaults); err != nil {
		return fmt.Errorf("defaults: %w", err)
	}
	if err := validateDNSSD(c.DNSSD); err != nil {
		return fmt.Errorf("dns_sd: %w", err)
	}
	if len(c.Printers) == 0 {
		return errors.New("at least one printer is required")
	}
	for name, printer := range c.Printers {
		if err := validateQueueName(name); err != nil {
			return fmt.Errorf("printers.%s: %w", name, err)
		}
		if printer.UpstreamURI == "" {
			return fmt.Errorf("printers.%s.upstream_uri is required", name)
		}
		if err := validateIPPURI(printer.UpstreamURI); err != nil {
			return fmt.Errorf("printers.%s.upstream_uri: %w", name, err)
		}
		if printer.RefreshRaw != "" && printer.RefreshInterval == 0 {
			return fmt.Errorf("printers.%s.refresh_interval is invalid", name)
		}
		if printer.RefreshRaw != "" {
			d, err := parseFlexibleDuration(printer.RefreshRaw)
			if err != nil || d <= 0 {
				return fmt.Errorf("printers.%s.refresh_interval must be positive", name)
			}
		}
		if !validIPPEverywhereMode(printer.IPPEverywhereMode) {
			return fmt.Errorf("printers.%s.ipp_everywhere_mode must be one of disabled, auto, required", name)
		}
		if printer.GeoLocation != "" {
			if err := validateGeoLocation(printer.GeoLocation); err != nil {
				return fmt.Errorf("printers.%s.geo_location: %w", name, err)
			}
		}
		if err := validatePassthrough(printer.Passthrough); err != nil {
			return fmt.Errorf("printers.%s.passthrough: %w", name, err)
		}
		if err := validatePolicyMedia(printer.Policy); err != nil {
			return fmt.Errorf("printers.%s.policy: %w", name, err)
		}
		if err := validatePolicyKeywords(printer.Policy); err != nil {
			return fmt.Errorf("printers.%s.policy: %w", name, err)
		}
		if printer.Policy.PrintColorMode == "" {
			return fmt.Errorf("printers.%s.policy.print_color_mode is required", name)
		}
		if printer.Policy.FidelityMode != "warn" && printer.Policy.FidelityMode != "reject" {
			return fmt.Errorf("printers.%s.policy.fidelity_mode must be warn or reject", name)
		}
	}
	return nil
}

func validatePolicyKeywords(p PolicyConfig) error {
	for _, item := range []struct {
		field string
		value string
	}{
		{field: "media_type", value: p.MediaType},
		{field: "print_color_mode", value: p.PrintColorMode},
	} {
		field, value := item.field, item.value
		if value == "" {
			continue
		}
		if err := validateKeyword(value); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	if p.MediaSource != nil {
		if err := validateKeyword(strings.TrimSpace(*p.MediaSource)); err != nil {
			return fmt.Errorf("media_source: %w", err)
		}
	}
	if p.PrintScaling != nil {
		if err := validateKeyword(strings.TrimSpace(*p.PrintScaling)); err != nil {
			return fmt.Errorf("print_scaling: %w", err)
		}
	}
	return nil
}

func validateDefaults(d DefaultsConfig) error {
	d.normalizeAliases()
	if d.MaxEnvelopeBytes <= 0 {
		return errors.New("max_envelope_bytes must be positive")
	}
	if d.MaxDocumentBytes <= 0 {
		return errors.New("max_document_bytes must be positive")
	}
	if d.MaxUpstreamResponseBytes <= 0 {
		return errors.New("max_upstream_response_bytes must be positive")
	}
	if d.MaxConcurrentPayloadJobsPerQueue <= 0 {
		return errors.New("max_concurrent_payload_jobs_per_queue must be positive")
	}
	if d.JobRetention <= 0 {
		return errors.New("job_retention must be positive")
	}
	if d.MaxEnvelopeBytes > d.MaxDocumentBytes {
		return errors.New("max_envelope_bytes must not exceed max_document_bytes")
	}
	return nil
}

func validateDNSSD(d DNSSDConfig) error {
	if d.Mode != DNSModeAuto && d.Mode != DNSModeOff {
		return errors.New("mode must be auto or off")
	}
	if d.Hostname != "" {
		if err := validateHostname(d.Hostname); err != nil {
			return fmt.Errorf("hostname: %w", err)
		}
	}
	if strings.IndexFunc(d.Interface, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("interface must not contain whitespace or control characters")
	}
	if len(d.Interface) > 255 {
		return errors.New("interface must not exceed 255 characters")
	}
	return nil
}

func validIPPEverywhereMode(mode string) bool {
	switch mode {
	case IPPEverywhereDisabled, IPPEverywhereAuto, IPPEverywhereRequired:
		return true
	default:
		return false
	}
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
			if err := validateKeyword(value); err != nil {
				return fmt.Errorf("media_supported[%d]: %w", i, err)
			}
			key := strings.ToLower(value)
			if _, ok := seen[key]; ok {
				return fmt.Errorf("media_supported contains duplicate value %q", raw)
			}
			seen[key] = struct{}{}
		}
		defaultValue := strings.ToLower(strings.TrimSpace(p.MediaDefault))
		if err := validateKeyword(strings.TrimSpace(p.MediaDefault)); err != nil {
			return fmt.Errorf("media_default: %w", err)
		}
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
	if err := validateKeyword(strings.TrimSpace(p.Media)); err != nil {
		return fmt.Errorf("media: %w", err)
	}
	return nil
}

func validatePassthrough(p PassthroughConfig) error {
	if err := validateKeywordList("preserve_job_attrs", p.PreserveJobAttrs); err != nil {
		return err
	}
	if err := validateKeywordList("drop_vendor_attrs", p.DropVendorAttrs); err != nil {
		return err
	}
	return nil
}

func validateKeywordList(field string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, raw := range values {
		value := strings.TrimSpace(raw)
		if err := validateKeyword(value); err != nil {
			return fmt.Errorf("%s[%d]: %w", field, i, err)
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("%s contains duplicate value %q", field, raw)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateQueueName(name string) error {
	if err := validateKeyword(name); err != nil {
		return fmt.Errorf("invalid queue name: %w", err)
	}
	first := name[0]
	if !isASCIIAlpha(first) {
		return errors.New("queue name must start with a letter")
	}
	return nil
}

func validateKeyword(value string) error {
	if value == "" {
		return errors.New("must not be empty")
	}
	if len(value) > 255 {
		return errors.New("must not exceed 255 characters")
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if isASCIIAlpha(ch) || isASCIIDigit(ch) || ch == '-' || ch == '_' || ch == '.' {
			continue
		}
		return fmt.Errorf("contains invalid keyword character %q", ch)
	}
	return nil
}

func isASCIIAlpha(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isASCIIDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func validateIPPURI(raw string) error {
	if strings.TrimSpace(raw) != raw || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("must not contain whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URI: %w", err)
	}
	if u.Scheme == "" || (strings.ToLower(u.Scheme) != "ipp" && strings.ToLower(u.Scheme) != "ipps") {
		return fmt.Errorf("scheme must be ipp or ipps")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return errors.New("must include a host")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
	}
	if u.Fragment != "" {
		return errors.New("must not contain a fragment")
	}
	return nil
}

func validateHostname(hostname string) error {
	value := strings.TrimSuffix(hostname, ".")
	if value == "" || len(value) > 253 {
		return errors.New("must be a valid hostname")
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("must be a valid hostname")
		}
		for i := 0; i < len(label); i++ {
			ch := label[i]
			if isASCIIAlpha(ch) || isASCIIDigit(ch) || ch == '-' {
				continue
			}
			return errors.New("must be a valid hostname")
		}
	}
	return nil
}

func validateGeoLocation(raw string) error {
	value := strings.TrimSpace(raw)
	if value != raw || !strings.HasPrefix(strings.ToLower(value), "geo:") {
		return errors.New("must be a geo URI (geo:latitude,longitude[,altitude])")
	}
	body := value[len("geo:"):]
	parts := strings.SplitN(body, ";", 2)
	coords := strings.Split(parts[0], ",")
	if len(coords) < 2 || len(coords) > 3 {
		return errors.New("must contain latitude and longitude")
	}
	lat, err := parseFiniteFloat(coords[0])
	if err != nil || lat < -90 || lat > 90 {
		return errors.New("latitude must be between -90 and 90")
	}
	lon, err := parseFiniteFloat(coords[1])
	if err != nil || lon < -180 || lon > 180 {
		return errors.New("longitude must be between -180 and 180")
	}
	if len(coords) == 3 {
		if _, err := parseFiniteFloat(coords[2]); err != nil {
			return errors.New("altitude must be a finite number")
		}
	}
	if len(parts) == 2 {
		if !strings.HasPrefix(parts[1], "u=") {
			return errors.New("uncertainty parameter must use u=")
		}
		uncertainty, err := parseFiniteFloat(strings.TrimPrefix(parts[1], "u="))
		if err != nil || uncertainty < 0 {
			return errors.New("uncertainty must be non-negative")
		}
	}
	return nil
}

func parseFiniteFloat(raw string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, errors.New("must be a finite number")
	}
	return value, nil
}

func parseByteLimitNode(node *yaml.Node) (int64, error) {
	if node.Kind != yaml.ScalarNode {
		return 0, errors.New("must be a byte count or size such as 1MiB")
	}
	return parseByteLimit(node.Value)
}

func parseByteLimit(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, errors.New("must not be empty")
	}
	units := []struct {
		suffix string
		factor uint64
	}{
		{suffix: "tib", factor: 1 << 40},
		{suffix: "tb", factor: 1_000_000_000_000},
		{suffix: "gib", factor: 1 << 30},
		{suffix: "gb", factor: 1_000_000_000},
		{suffix: "mib", factor: 1 << 20},
		{suffix: "mb", factor: 1_000_000},
		{suffix: "kib", factor: 1 << 10},
		{suffix: "kb", factor: 1_000},
		{suffix: "b", factor: 1},
	}
	lower := strings.ToLower(value)
	factor := uint64(1)
	number := value
	for _, unit := range units {
		if strings.HasSuffix(lower, unit.suffix) {
			factor = unit.factor
			number = strings.TrimSpace(value[:len(value)-len(unit.suffix)])
			break
		}
	}
	if number == "" {
		return 0, errors.New("must contain a number")
	}
	amount, err := strconv.ParseFloat(number, 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return 0, errors.New("must be a non-negative byte count")
	}
	bytes := amount * float64(factor)
	if bytes > float64(math.MaxInt64) || math.Trunc(bytes) != bytes {
		return 0, errors.New("byte count overflows an integer or is fractional")
	}
	return int64(bytes), nil
}

func parsePositiveIntNode(node *yaml.Node) (int, error) {
	if node.Kind != yaml.ScalarNode {
		return 0, errors.New("must be a positive integer")
	}
	value, err := strconv.ParseInt(strings.TrimSpace(node.Value), 10, 64)
	if err != nil || value > int64(math.MaxInt) {
		return 0, errors.New("must be a positive integer")
	}
	return int(value), nil
}

func parseDurationNode(node *yaml.Node) (time.Duration, error) {
	if node.Kind != yaml.ScalarNode {
		return 0, errors.New("must be a duration")
	}
	return parseFlexibleDuration(node.Value)
}

func parseFlexibleDuration(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, errors.New("must be a duration")
	}
	if strings.HasSuffix(strings.ToLower(value), "d") {
		days, err := strconv.ParseFloat(strings.TrimSpace(value[:len(value)-1]), 64)
		if err != nil || math.IsNaN(days) || math.IsInf(days, 0) {
			return 0, errors.New("must be a valid duration")
		}
		hours := days * float64(24*time.Hour)
		if hours > float64(math.MaxInt64) || hours < float64(math.MinInt64) {
			return 0, errors.New("duration overflows time.Duration")
		}
		return time.Duration(hours), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a valid duration: %w", err)
	}
	return duration, nil
}

type yamlSchema struct {
	fields  map[string]*yamlSchema
	dynamic bool
}

func objectSchema(fields map[string]*yamlSchema) *yamlSchema {
	return &yamlSchema{fields: fields}
}

func dynamicSchema(child *yamlSchema) *yamlSchema {
	return &yamlSchema{fields: child.fields, dynamic: true}
}

func warnUnknownKeys(node *yaml.Node, logger *slog.Logger) {
	if logger == nil {
		return
	}
	schema := configYAMLSchema()
	walkUnknownKeys(node, schema, "", logger)
}

func walkUnknownKeys(node *yaml.Node, schema *yamlSchema, path string, logger *slog.Logger) {
	if node == nil || schema == nil {
		return
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) > 0 {
			walkUnknownKeys(node.Content[0], schema, path, logger)
		}
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		fullPath := key
		if path != "" {
			fullPath = path + "." + key
		}
		child, ok := schema.fields[key]
		if !ok {
			logger.Warn("unknown configuration key", "key", fullPath)
			continue
		}
		if fullPath == "passthrough.allow_unknown_attributes" || strings.HasSuffix(fullPath, ".passthrough.allow_unknown_attributes") {
			logger.Warn("deprecated configuration key", "key", fullPath, "reason", "the setting is ignored")
		}
		if child.dynamic {
			walkDynamicKeys(node.Content[i+1], child, fullPath, logger)
		} else {
			walkUnknownKeys(node.Content[i+1], child, fullPath, logger)
		}
	}
}

func walkDynamicKeys(node *yaml.Node, schema *yamlSchema, path string, logger *slog.Logger) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		queue := node.Content[i].Value
		queuePath := path + "." + queue
		walkUnknownKeys(node.Content[i+1], &yamlSchema{fields: schema.fields}, queuePath, logger)
	}
}

func configYAMLSchema() *yamlSchema {
	keyword := &yamlSchema{}
	policy := objectSchema(map[string]*yamlSchema{
		"media":            keyword,
		"media_supported":  keyword,
		"media_default":    keyword,
		"media_type":       keyword,
		"print_color_mode": keyword,
		"media_source":     keyword,
		"print_scaling":    keyword,
		"use_media_col":    keyword,
		"fidelity_mode":    keyword,
	})
	passthrough := objectSchema(map[string]*yamlSchema{
		"allow_unknown_attributes": keyword,
		"preserve_job_attrs":       keyword,
		"drop_vendor_attrs":        keyword,
	})
	printer := objectSchema(map[string]*yamlSchema{
		"upstream_uri":        keyword,
		"display_name":        keyword,
		"location":            keyword,
		"optional":            keyword,
		"ipp_everywhere_mode": keyword,
		"dns_sd":              keyword,
		"geo_location":        keyword,
		"refresh_interval":    keyword,
		"policy":              policy,
		"passthrough":         passthrough,
	})
	defaults := objectSchema(map[string]*yamlSchema{
		"max_envelope_bytes":                    keyword,
		"envelope":                              keyword,
		"envelope_max_bytes":                    keyword,
		"max_document_bytes":                    keyword,
		"document":                              keyword,
		"document_max_bytes":                    keyword,
		"max_upstream_response_bytes":           keyword,
		"upstream_response":                     keyword,
		"upstream_response_max_bytes":           keyword,
		"max_concurrent_payload_jobs_per_queue": keyword,
		"concurrent_payload_jobs_per_queue":     keyword,
		"job_retention":                         keyword,
		"retention":                             keyword,
	})
	return objectSchema(map[string]*yamlSchema{
		"listen": objectSchema(map[string]*yamlSchema{
			"addr":            keyword,
			"public_base_url": keyword,
		}),
		"storage": objectSchema(map[string]*yamlSchema{
			"sqlite_path": keyword,
		}),
		"dns_sd": objectSchema(map[string]*yamlSchema{
			"mode":      keyword,
			"hostname":  keyword,
			"interface": keyword,
		}),
		"defaults": defaults,
		"limits":   defaults,
		"printers": dynamicSchema(printer),
	})
}
