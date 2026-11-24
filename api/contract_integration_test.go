//go:build integration

package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/contractenv"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// runPass runs the shared contract suite against the real service on a fresh database: the same suite,
// examples and skips as for the double (double/contract_test.go).
func runPass(t *testing.T, world bool, skip func(spec string) map[string]string) {
	spec := contractenv.SpecPath(t)
	s := itest.New(t)
	if world {
		s.Seed(t)
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &double.ContractTransport{
		Tenant: func() string { return s.TenantToken(seed.Org) },
		Vendor: func() http.Header {
			return s.VendorHeaders("admin.org.read", "admin.org.create", "admin.registration.verify")
		},
		Svc: func(string) http.Header { return s.ServiceHeaders("identity", seed.Org) },
	}}
	contracttest.Run(t, contracttest.Options{SpecPath: spec, BaseURL: s.Srv.URL, Client: client, Skip: skip(spec)})
}

func TestRealServiceContractExampleWorld(t *testing.T) {
	runPass(t, true, func(string) map[string]string { return contractenv.SkipExampleWorld() })
}

func TestRealServiceContractOnboarding(t *testing.T) {
	runPass(t, false, func(spec string) map[string]string { return contractenv.SkipAllExcept(t, spec, "createOrganisation") })
}
