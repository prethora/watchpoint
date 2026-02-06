package types

import "time"

// PlanLimits defines the resource constraints for an organization's billing plan.
type PlanLimits struct {
	MaxWatchPoints   int  `json:"watchpoints_max"`
	MaxAPICallsDaily int  `json:"api_calls_daily_max"`
	AllowNowcast     bool `json:"allow_nowcast"`
}

// TimeWindow defines a start/end time range for event-mode WatchPoints.
type TimeWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// MonitorConfig defines rolling-window parameters for monitor-mode WatchPoints.
type MonitorConfig struct {
	WindowHours int      `json:"window_hours" validate:"required,min=6,max=168"`
	ActiveHours [][2]int `json:"active_hours"`
	ActiveDays  []int    `json:"active_days"`
}

// NotificationPreferences contains organization-level notification settings.
type NotificationPreferences struct {
	QuietHours *QuietHoursConfig `json:"quiet_hours,omitempty"`
	Digest     *DigestConfig     `json:"digest,omitempty"`
}

// QuietHoursConfig defines when notifications should be suppressed.
type QuietHoursConfig struct {
	Enabled  bool          `json:"enabled"`
	Schedule []QuietPeriod `json:"schedule"`
	Timezone string        `json:"timezone"`
}

// QuietPeriod defines a recurring quiet window.
type QuietPeriod struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
}

// DigestConfig defines how notification digests are generated and delivered.
type DigestConfig struct {
	Enabled      bool   `json:"enabled"`
	Frequency    string `json:"frequency"`
	DeliveryTime string `json:"delivery_time"`
	SendEmpty    bool   `json:"send_empty"`
	TemplateSet  string `json:"template_set"`
}

// Preferences contains per-WatchPoint notification behavior settings.
type Preferences struct {
	NotifyOnClear          bool `json:"notify_on_clear"`
	NotifyOnForecastChange bool `json:"notify_on_forecast_change"`
}

// RedirectURLs is used by the BillingService to guide the user after Stripe checkout.
type RedirectURLs struct {
	Success string
	Cancel  string
}
