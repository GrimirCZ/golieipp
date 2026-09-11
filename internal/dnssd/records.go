// Package dnssd contains the small boundary between the proxy and a DNS-SD
// implementation. It deliberately deals in printer publication inputs and
// status, rather than IPP capability objects.
package dnssd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const DefaultCollisionRetries = 3

// ServiceInput is the complete caller-owned description needed to publish a
// proxy queue. TXT is copied and never synthesized from printer capabilities.
type ServiceInput struct {
	Name      string
	Hostname  string
	Port      uint16
	IPPS      bool
	Interface string
	TXT       map[string]string
	// GeoLocation is optional RFC 1876 location data expressed as a geo URI.
	// It is carried through the seam so an adapter can publish a LOC record;
	// TXT remains entirely caller-owned.
	GeoLocation string

	// MaxCollisionRetries bounds alternate-name attempts. Zero uses
	// DefaultCollisionRetries; negative values are rejected.
	MaxCollisionRetries int
}

// PublishRequest and ServiceInfo are descriptive aliases kept for callers
// that use those terms for the same publication input.
type PublishRequest = ServiceInput
type ServiceInfo = ServiceInput

// ServiceRecord is a pure description of one DNS-SD service registration.
// Subtype is empty for a regular service and non-empty for a subtype record.
type ServiceRecord struct {
	Name        string
	Hostname    string
	Port        uint16
	Type        string
	Subtype     string
	TXT         map[string]string
	GeoLocation string
}

func (r ServiceRecord) FullType() string {
	if r.Subtype == "" {
		return r.Type
	}
	return r.Subtype + "._sub." + strings.TrimPrefix(r.Type, ".")
}

// BuildRecords returns the registrations required for an IPP Everywhere
// queue. The _universal subtype is used by AirPrint clients, while _print and
// the legacy _printer._tcp registration preserve discovery by other clients.
// The legacy registration intentionally uses port zero so it does not
// advertise a second transport endpoint.
func BuildRecords(input ServiceInput) ([]ServiceRecord, error) {
	if err := validateServiceInput(input); err != nil {
		return nil, err
	}
	txt := cloneTXT(input.TXT)
	ipp := []ServiceRecord{
		{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipp._tcp", TXT: cloneTXT(txt), GeoLocation: input.GeoLocation},
		{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipp._tcp", Subtype: "_print"},
		{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipp._tcp", Subtype: "_universal"},
		{Name: input.Name, Hostname: input.Hostname, Port: 0, Type: "_printer._tcp"},
	}
	if input.IPPS {
		ipp = append(ipp,
			ServiceRecord{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipps._tcp", TXT: cloneTXT(txt)},
			ServiceRecord{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipps._tcp", Subtype: "_print"},
			ServiceRecord{Name: input.Name, Hostname: input.Hostname, Port: input.Port, Type: "_ipps._tcp", Subtype: "_universal"},
		)
	}
	return ipp, nil
}

func validateServiceInput(input ServiceInput) error {
	if input.Name == "" {
		return errors.New("service name is required")
	}
	if len([]byte(input.Name)) > 63 {
		return errors.New("service name must not exceed 63 bytes")
	}
	if strings.IndexFunc(input.Name, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return errors.New("service name must not contain control characters")
	}
	if input.Port == 0 {
		return errors.New("service port must be positive")
	}
	if input.MaxCollisionRetries < 0 {
		return errors.New("max collision retries must not be negative")
	}
	if input.GeoLocation != "" {
		if _, err := EncodeLOC(input.GeoLocation); err != nil {
			return fmt.Errorf("geo location: %w", err)
		}
	}
	for key, value := range input.TXT {
		if key == "" || strings.Contains(key, "=") {
			return fmt.Errorf("TXT key %q is invalid", key)
		}
		if len([]byte(key)) > 255 || len([]byte(key))+1+len([]byte(value)) > 255 {
			return fmt.Errorf("TXT entry %q exceeds 255 bytes", key)
		}
		if strings.IndexFunc(key, func(r rune) bool { return unicode.IsControl(r) }) >= 0 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
			return fmt.Errorf("TXT entry %q contains a control character", key)
		}
	}
	if err := validateTXTSize(input.TXT); err != nil {
		return err
	}
	return nil
}

const (
	maxTXTEntryBytes = 255
	maxTXTDataBytes  = 1300
	maxRPBytes       = 400
)

var txtKeyOrder = []string{
	"rp", "txtvers", "qtotal", "priority", "note", "air", "tls",
	"adminurl", "uuid", "duuid", "ty", "color", "duplex", "copies", "collate",
	"papermax", "papercustom", "bind", "punch", "sort", "staple", "product", "pdl", "urf",
}

func txtKeyPriority(key string) int {
	key = strings.ToLower(key)
	for index, preferred := range txtKeyOrder {
		if key == preferred {
			return index
		}
	}
	return len(txtKeyOrder)
}

func validateTXTSize(txt map[string]string) error {
	entries := TXTEntries(txt)
	total := 0
	rpEnd := 0
	for _, entry := range entries {
		entryBytes := len([]byte(entry))
		if entryBytes > maxTXTEntryBytes {
			return fmt.Errorf("TXT entry exceeds %d bytes", maxTXTEntryBytes)
		}
		total += 1 + entryBytes // one length byte precedes each DNS-SD string.
		if strings.HasPrefix(strings.ToLower(entry), "rp=") && rpEnd == 0 {
			rpEnd = total
		}
	}
	if total > maxTXTDataBytes {
		return fmt.Errorf("TXT data exceeds %d bytes", maxTXTDataBytes)
	}
	if rpEnd > maxRPBytes {
		return fmt.Errorf("TXT rp entry must occur within the first %d bytes", maxRPBytes)
	}
	return nil
}

// EncodeLOC converts a geo URI to the 16-byte RFC 1876 LOC RDATA payload
// accepted by Avahi's EntryGroup.AddRecord call. The default size and
// precision values are the RFC's conservative 1m/10km/10m values.
func EncodeLOC(raw string) ([]byte, error) {
	lat, lon, altitude, err := parseGeoLocation(raw)
	if err != nil {
		return nil, err
	}
	if altitude < -100000 || altitude > 42849672.95 {
		return nil, errors.New("altitude is outside the LOC range")
	}
	data := make([]byte, 16)
	data[0] = 0 // LOC version
	data[1] = 0x12
	data[2] = 0x16
	data[3] = 0x13
	// LOC coordinates are unsigned milliarcseconds offset from -90/-180.
	binary.BigEndian.PutUint32(data[4:8], uint32(math.Round((lat+90)*3600000)))
	binary.BigEndian.PutUint32(data[8:12], uint32(math.Round((lon+180)*3600000)))
	// Altitude is unsigned centimetres offset by -100000m.
	binary.BigEndian.PutUint32(data[12:16], uint32(math.Round((altitude+100000)*100)))
	return data, nil
}

func parseGeoLocation(raw string) (float64, float64, float64, error) {
	value := strings.TrimSpace(raw)
	if value != raw || !strings.HasPrefix(strings.ToLower(value), "geo:") {
		return 0, 0, 0, errors.New("must be a geo URI (geo:latitude,longitude[,altitude])")
	}
	parts := strings.SplitN(value[len("geo:"):], ";", 2)
	coords := strings.Split(parts[0], ",")
	if len(coords) < 2 || len(coords) > 3 {
		return 0, 0, 0, errors.New("must contain latitude and longitude")
	}
	parse := func(raw string) (float64, error) {
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, errors.New("coordinates must be finite numbers")
		}
		return value, nil
	}
	lat, err := parse(coords[0])
	if err != nil || lat < -90 || lat > 90 {
		return 0, 0, 0, errors.New("latitude must be between -90 and 90")
	}
	lon, err := parse(coords[1])
	if err != nil || lon < -180 || lon > 180 {
		return 0, 0, 0, errors.New("longitude must be between -180 and 180")
	}
	altitude := float64(0)
	if len(coords) == 3 {
		altitude, err = parse(coords[2])
		if err != nil {
			return 0, 0, 0, errors.New("altitude must be a finite number")
		}
	}
	if len(parts) == 2 {
		if !strings.HasPrefix(parts[1], "u=") {
			return 0, 0, 0, errors.New("uncertainty parameter must use u=")
		}
		uncertainty, err := parse(parts[1][2:])
		if err != nil || uncertainty < 0 {
			return 0, 0, 0, errors.New("uncertainty must be non-negative")
		}
	}
	return lat, lon, altitude, nil
}

func cloneTXT(txt map[string]string) map[string]string {
	if len(txt) == 0 {
		return nil
	}
	copyTXT := make(map[string]string, len(txt))
	for key, value := range txt {
		copyTXT[key] = value
	}
	return copyTXT
}

// TXTEntries converts caller-owned key/value TXT data to deterministic
// key=value strings. Routing and version keys are deliberately prioritized so
// clients can find rp within the early portion of the DNS-SD TXT payload.
func TXTEntries(txt map[string]string) []string {
	entries := make([]string, 0, len(txt))
	keys := make([]string, 0, len(txt))
	for key := range txt {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		leftPriority, rightPriority := txtKeyPriority(keys[i]), txtKeyPriority(keys[j])
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		left, right := strings.ToLower(keys[i]), strings.ToLower(keys[j])
		if left != right {
			return left < right
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys {
		entries = append(entries, key+"="+txt[key])
	}
	return entries
}

// AlternateServiceName follows Avahi's conventional collision suffixes:
// attempt zero is the requested name, then " (2)", " (3)", and so on.
func AlternateServiceName(name string, attempt int) string {
	if attempt <= 0 {
		return name
	}
	return fmt.Sprintf("%s (%d)", name, attempt+1)
}

// CollisionNames returns at most maxAttempts candidate names. Non-positive
// limits produce no candidates; callers can use DefaultCollisionRetries when
// they want the package default.
func CollisionNames(name string, maxAttempts int) []string {
	if maxAttempts <= 0 {
		return nil
	}
	names := make([]string, 0, maxAttempts)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		names = append(names, AlternateServiceName(name, attempt))
	}
	return names
}
