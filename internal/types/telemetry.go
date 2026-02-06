package types

// Telemetry metric names for CloudWatch.
// All components MUST use these constants.
const (
	// Metric Names
	MetricForecastReady       = "ForecastReady"
	MetricEvaluationLag       = "EvaluationLag"
	MetricNotificationFailure = "NotificationFailure"
	MetricBillingWarning      = "BillingWarning"
	MetricAPILatency          = "APILatency"
	MetricExternalAPIFailure  = "ExternalAPIFailure"
	MetricDeliveryAttempt     = "DeliveryAttempt"
	MetricDeliverySuccess     = "DeliverySuccess"
	MetricDeliveryFailed      = "DeliveryFailed"

	// Dimension Keys
	DimForecastType = "ForecastType"
	DimQueue        = "Queue"
	DimChannel      = "Channel"
	DimOrgID        = "OrgID"
	DimEndpoint     = "Endpoint"
	DimProvider     = "Provider"
	DimEventType    = "EventType"

	// Metric Namespace
	MetricNamespace = "WatchPoint"
)

// Zarr Schema Variables.
// Canonical variable names for forecast data. The Python Eval Worker MUST use these exact keys.
const (
	ZarrVarTemperatureC      = "temperature_c"
	ZarrVarPrecipitationMM   = "precipitation_mm"
	ZarrVarPrecipitationProb = "precipitation_probability"
	ZarrVarWindSpeedKmh      = "wind_speed_kmh"
	ZarrVarHumidityPercent   = "humidity_percent"
	ZarrVarPressureHPa       = "pressure_hpa"
	ZarrVarCloudCoverPercent = "cloud_cover_percent"
	ZarrVarUVIndex           = "uv_index"
)
