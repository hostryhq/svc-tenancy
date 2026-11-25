// Package config reads the service configuration from the environment (12-factor) and fails fast with
// every problem at once. Secrets are database credentials and key material by reference only: a DSN or
// a file path, never a literal in code or logs.
package config

import (
	"time"

	"github.com/hostryhq/go-common/config"
)

// Config is the validated configuration.
type Config struct {
	Addr string
	// AppDSN connects as tenancy_app, VendorDSN as tenancy_vendor, RelayDSN as tenancy_relay (outbox only).
	AppDSN, VendorDSN, RelayDSN string
	// MigrateDSN connects as the migrator; used only by `tenancy migrate`.
	MigrateDSN string
	// Key sets: JWKS documents (URL or file path) of the tenant and vendor token issuers, and the service
	// key registry (a JWKS file whose kid is the service name).
	TenantJWKS, VendorJWKS, ServiceKeys string
	// SelfName is this service's name in service tokens (aud).
	SelfName string
	// Issuer and audience names of the two token realms (ADR-0005).
	TenantIssuer, TenantAudience, VendorIssuer, VendorAudience string
	// LinksEnabled registers the tenant-facing link routes (default false).
	LinksEnabled bool
	// NatsURL and NatsSeedFile connect the outbox relay to the bus; empty disables publishing.
	NatsURL, NatsSeedFile string
	KeyRefresh            time.Duration
	LogLevel              string
}

// Load reads the environment through lookup.
func Load(lookup config.Lookup) (Config, error) {
	l := config.New(lookup)
	c := Config{
		Addr:           l.String("TENANCY_ADDR", ":8080"),
		AppDSN:         l.Required("TENANCY_DB_APP_DSN"),
		VendorDSN:      l.Required("TENANCY_DB_VENDOR_DSN"),
		RelayDSN:       l.String("TENANCY_DB_RELAY_DSN", ""),
		MigrateDSN:     l.String("TENANCY_DB_MIGRATE_DSN", ""),
		TenantJWKS:     l.Required("TENANCY_TENANT_JWKS"),
		VendorJWKS:     l.Required("TENANCY_VENDOR_JWKS"),
		ServiceKeys:    l.Required("TENANCY_SERVICE_KEYS"),
		SelfName:       l.String("TENANCY_SELF_NAME", "tenancy"),
		TenantIssuer:   l.String("TENANCY_TENANT_ISSUER", "hostry-identity"),
		TenantAudience: l.String("TENANCY_TENANT_AUDIENCE", "hostry-tenant"),
		VendorIssuer:   l.String("TENANCY_VENDOR_ISSUER", "hostry-identity-vendor"),
		VendorAudience: l.String("TENANCY_VENDOR_AUDIENCE", "hostry-vendor"),
		LinksEnabled:   l.Bool("LINKS_ENABLED", false),
		NatsURL:        l.String("NATS_URL", ""),
		NatsSeedFile:   l.String("NATS_SEED_FILE", ""),
		KeyRefresh:     l.Duration("TENANCY_KEY_REFRESH", time.Minute),
		LogLevel:       l.String("LOG_LEVEL", "info"),
	}
	return c, l.Err()
}
