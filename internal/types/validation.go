package types

import (
	"fmt"
	"net/url"
	"time"
)

// Validation constraint constants.
const (
	MinLat           = -90.0
	MaxLat           = 90.0
	MinLon           = -180.0
	MaxLon           = 180.0
	MaxConditions    = 10
	MaxNameLength    = 200
	ConusMinLat      = 24.0
	ConusMaxLat      = 50.0
	ConusMinLon      = -125.0
	ConusMaxLon      = -66.0
	MaxMonitorWindow = 168
	MinMonitorWindow = 6
)

// IsCONUS returns true if the coordinates fall within the CONUS bounding box.
func IsCONUS(lat, lon float64) bool {
	return lat >= ConusMinLat && lat <= ConusMaxLat && lon >= ConusMinLon && lon <= ConusMaxLon
}

// VariableMetadata defines the canonical rules for a weather variable.
type VariableMetadata struct {
	ID          string     `json:"id"`
	Unit        string     `json:"unit"`
	Range       [2]float64 `json:"valid_range"`
	Description string     `json:"description"`
}

// StandardVariables defines the authoritative constraints for the platform.
// All components MUST validate against these ranges.
var StandardVariables = map[string]VariableMetadata{
	"temperature_c":             {ID: "temperature_c", Unit: "celsius", Range: [2]float64{-60, 60}, Description: "Air temperature at 2m above ground level"},
	"precipitation_probability": {ID: "precipitation_probability", Unit: "percent", Range: [2]float64{0, 100}, Description: "Probability of precipitation"},
	"precipitation_mm":          {ID: "precipitation_mm", Unit: "mm", Range: [2]float64{0, 500}, Description: "Accumulated precipitation"},
	"wind_speed_kmh":            {ID: "wind_speed_kmh", Unit: "kmh", Range: [2]float64{0, 300}, Description: "Wind speed at 10m above ground level"},
	"humidity_percent":          {ID: "humidity_percent", Unit: "percent", Range: [2]float64{0, 100}, Description: "Relative humidity"},
	"cloud_cover_percent":       {ID: "cloud_cover_percent", Unit: "percent", Range: [2]float64{0, 100}, Description: "Cloud cover percentage"},
	"uv_index":                  {ID: "uv_index", Unit: "index", Range: [2]float64{0, 15}, Description: "UV radiation index"},
}

// ValidateConditionThreshold checks if a threshold value is within the valid range for its variable.
func ValidateConditionThreshold(variable string, threshold float64) error {
	meta, ok := StandardVariables[variable]
	if !ok {
		return fmt.Errorf("%s: unknown variable '%s'", ErrCodeValidationInvalidConditions, variable)
	}
	if threshold < meta.Range[0] || threshold > meta.Range[1] {
		return fmt.Errorf("%s: threshold %.2f outside valid range [%.2f, %.2f] for %s",
			ErrCodeValidationThresholdRange, threshold, meta.Range[0], meta.Range[1], variable)
	}
	return nil
}

// ValidateTimeWindow ensures End > Start and applies business rules.
func ValidateTimeWindow(tw *TimeWindow) error {
	if tw == nil {
		return nil
	}
	if !tw.End.After(tw.Start) {
		return fmt.Errorf("%s: end must be after start", ErrCodeValidationTimeWindow)
	}
	// Maximum duration = 30 days
	if tw.End.Sub(tw.Start) > 30*24*time.Hour {
		return fmt.Errorf("%s: maximum window is 30 days", ErrCodeValidationTimeWindow)
	}
	return nil
}

// ValidateWebhookURL checks that a URL is safe for webhook delivery.
func ValidateWebhookURL(urlStr string) error {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", ErrCodeValidationInvalidWebhook)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%s: must use HTTPS", ErrCodeValidationInvalidWebhook)
	}
	// SSRF check is performed at delivery time, not validation
	return nil
}

// SSRFBlockedCIDRs defines the IP ranges that MUST be blocked for SSRF protection.
var SSRFBlockedCIDRs = []string{
	"127.0.0.0/8",     // Localhost
	"10.0.0.0/8",      // Private Class A
	"172.16.0.0/12",   // Private Class B
	"192.168.0.0/16",  // Private Class C
	"169.254.0.0/16",  // Link-local (AWS Metadata!)
	"0.0.0.0/8",       // Current network
	"224.0.0.0/4",     // Multicast
	"240.0.0.0/4",     // Reserved
	"100.64.0.0/10",   // Shared Address Space (CGN)
	"198.18.0.0/15",   // Benchmark testing
	"fc00::/7",        // IPv6 private
	"fe80::/10",       // IPv6 link-local
	"::1/128",         // IPv6 localhost
}
