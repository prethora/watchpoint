package types

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Compile-time interface assertions.
// These ensure all JSONB types implement both sql.Scanner and driver.Valuer,
// catching any method signature drift at compile time rather than at runtime.
// Scan is on pointer receivers; Value is on value receivers.
var (
	_ sql.Scanner   = (*ChannelList)(nil)
	_ driver.Valuer = ChannelList(nil)
	_ sql.Scanner   = (*MonitorConfig)(nil)
	_ driver.Valuer = MonitorConfig{}
	_ sql.Scanner   = (*PlanLimits)(nil)
	_ driver.Valuer = PlanLimits{}
	_ sql.Scanner   = (*NotificationPreferences)(nil)
	_ driver.Valuer = NotificationPreferences{}
	_ sql.Scanner   = (*Preferences)(nil)
	_ driver.Valuer = Preferences{}
	_ sql.Scanner   = (*EvaluationResult)(nil)
	_ driver.Valuer = EvaluationResult{}
	// Conditions assertions are in conditions.go (already implemented).
)

// scanJSONB is a generic helper that scans a JSONB database value into a Go pointer.
// It handles nil values, []byte, and string representations from different database drivers.
func scanJSONB(dest interface{}, value interface{}) error {
	if value == nil {
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("jsonb: unsupported scan type %T", value)
	}
	return json.Unmarshal(data, dest)
}

// valueJSONB is a generic helper that converts a Go value to a JSONB-compatible driver.Value.
// Returns nil for nil interface values; otherwise marshals to JSON bytes.
func valueJSONB(v interface{}) (driver.Value, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// ---------------------------------------------------------------------------
// ChannelList
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (cl *ChannelList) Scan(value interface{}) error {
	if value == nil {
		*cl = nil
		return nil
	}
	return scanJSONB(cl, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
// Note: This writes the full config including secrets. The custom MarshalJSON on
// Channel handles redaction only for API response serialization, not database storage.
func (cl ChannelList) Value() (driver.Value, error) {
	if cl == nil {
		return nil, nil
	}
	// We must bypass the redacting MarshalJSON on Channel by using an alias type.
	type channelAlias struct {
		ID      string         `json:"id"`
		Type    ChannelType    `json:"type"`
		Config  map[string]any `json:"config"`
		Enabled bool           `json:"enabled"`
	}
	aliases := make([]channelAlias, len(cl))
	for i, ch := range cl {
		aliases[i] = channelAlias{
			ID:      ch.ID,
			Type:    ch.Type,
			Config:  ch.Config,
			Enabled: ch.Enabled,
		}
	}
	return json.Marshal(aliases)
}

// ---------------------------------------------------------------------------
// MonitorConfig
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (mc *MonitorConfig) Scan(value interface{}) error {
	return scanJSONB(mc, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (mc MonitorConfig) Value() (driver.Value, error) {
	return valueJSONB(mc)
}

// ---------------------------------------------------------------------------
// PlanLimits
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (pl *PlanLimits) Scan(value interface{}) error {
	return scanJSONB(pl, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (pl PlanLimits) Value() (driver.Value, error) {
	return valueJSONB(pl)
}

// ---------------------------------------------------------------------------
// NotificationPreferences
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (np *NotificationPreferences) Scan(value interface{}) error {
	return scanJSONB(np, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (np NotificationPreferences) Value() (driver.Value, error) {
	return valueJSONB(np)
}

// ---------------------------------------------------------------------------
// Preferences (per-WatchPoint notification preferences)
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (p *Preferences) Scan(value interface{}) error {
	return scanJSONB(p, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (p Preferences) Value() (driver.Value, error) {
	return valueJSONB(p)
}

// ---------------------------------------------------------------------------
// EvaluationResult
// ---------------------------------------------------------------------------

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (er *EvaluationResult) Scan(value interface{}) error {
	return scanJSONB(er, value)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (er EvaluationResult) Value() (driver.Value, error) {
	return valueJSONB(er)
}
