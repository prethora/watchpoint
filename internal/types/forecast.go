package types

import "time"

// ForecastSnapshot contains the weather data values at a point in time and space.
type ForecastSnapshot struct {
	PrecipitationProb float64 `json:"precipitation_probability"`
	PrecipitationMM   float64 `json:"precipitation_mm"`
	TemperatureC      float64 `json:"temperature_c"`
	WindSpeedKmh      float64 `json:"wind_speed_kmh"`
	Humidity          float64 `json:"humidity_percent"`
}

// EvaluationResult captures the outcome of evaluating a WatchPoint's conditions
// against forecast data.
type EvaluationResult struct {
	Timestamp         time.Time        `json:"timestamp"`
	Triggered         bool             `json:"triggered"`
	MatchedConditions []Condition      `json:"matched_conditions,omitempty"`
	ForecastSnapshot  ForecastSnapshot `json:"forecast_snapshot"`
	ModelUsed         string           `json:"model_used"`
}
