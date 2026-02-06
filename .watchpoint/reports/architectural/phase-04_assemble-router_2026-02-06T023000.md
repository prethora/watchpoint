# Architectural Suggestion: Missing Config Fields Referenced in Router Middleware

## Context
While implementing the "Assemble Router and Mount Routes" task in Phase 4, I needed to wire `registerGlobalMiddleware()` according to the architecture spec (05a-api-core.md Section 4.1).

## The Issue
The architecture spec references two Config fields that do not exist in the actual Config struct (03-config.md / internal/config/config.go):

1. **`s.Config.RequestTimeout`** -- used in `ContextTimeoutMiddleware(s.Config.RequestTimeout)`. There is no `RequestTimeout` field on the Config struct or any sub-struct.

2. **`s.Config.RedactedHeaders`** -- used in `RequestLogger(s.Logger, s.Config.RedactedHeaders)`. There is no `RedactedHeaders` field on the Config struct or any sub-struct.

3. **`s.Config.CorsAllowedOrigins`** -- the spec uses this flat reference but the actual config has it at `s.Config.Security.CorsAllowedOrigins`. This is a minor discrepancy in the spec's example code.

## Why It Matters
Without these Config fields, the middleware configuration cannot be externalized. Currently:
- Request timeout is hardcoded to 29 seconds (Lambda default minus 1s).
- Redacted headers are hardcoded to `["Authorization", "Cookie", "X-CSRF-Token"]`.

For production operation, especially the request timeout, this should be configurable per environment (different Lambda timeout configurations).

## Which Architecture Files Are Affected
- `architecture/03-config.md` -- needs `RequestTimeout` and `RedactedHeaders` fields added
- `architecture/05a-api-core.md` Section 4.1 -- references `s.Config.RequestTimeout` and `s.Config.RedactedHeaders` (should match whatever is added to 03-config.md)

## Suggested Resolution
Add to `ServerConfig` in `03-config.md`:
```go
type ServerConfig struct {
    Port            string        `envconfig:"PORT" default:"8080"`
    APIExternalURL  string        `envconfig:"API_EXTERNAL_URL" validate:"required,url"`
    DashboardURL    string        `envconfig:"DASHBOARD_URL" validate:"required,url"`
    RequestTimeout  time.Duration `envconfig:"REQUEST_TIMEOUT" default:"29s"`
    RedactedHeaders []string      `envconfig:"REDACTED_HEADERS" default:"Authorization,Cookie,X-CSRF-Token"`
}
```

Then in 05a-api-core.md Section 4.1, update references to:
- `s.Config.Server.RequestTimeout`
- `s.Config.Server.RedactedHeaders`
- `s.Config.Security.CorsAllowedOrigins`

## Impact on Current Task
This did NOT prevent completion. I created helper methods (`requestTimeout()`, `redactedHeaders()`, `corsAllowedOrigins()`) that provide sensible defaults and include TODO comments indicating where to wire config fields when they are added. The implementation is fully functional and testable with the current defaults.
