package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// Condition defines a threshold-based rule for a weather variable.
type Condition struct {
	Variable  string            `json:"variable"`
	Operator  ConditionOperator `json:"operator"`
	Threshold []float64         `json:"threshold"`
	Unit      string            `json:"unit"`
}

// Conditions is a slice of Condition that implements sql.Scanner and driver.Valuer
// for JSONB column storage.
type Conditions []Condition

// Scan implements the sql.Scanner interface for reading JSONB from the database.
func (c *Conditions) Scan(value interface{}) error {
	if value == nil {
		*c = nil
		return nil
	}
	var data []byte
	switch v := value.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return fmt.Errorf("conditions: unsupported scan type %T", value)
	}
	return json.Unmarshal(data, c)
}

// Value implements the driver.Valuer interface for writing JSONB to the database.
func (c Conditions) Value() (driver.Value, error) {
	if c == nil {
		return nil, nil
	}
	return json.Marshal(c)
}
