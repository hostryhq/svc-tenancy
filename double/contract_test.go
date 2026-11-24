package double_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/contractenv"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// runPass runs the shared contract suite against a fresh double.
func runPass(t *testing.T, world bool, skip func(spec string) map[string]string) {
	spec := contractenv.SpecPath(t)
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	if world {
		if err := env.SeedExampleWorld(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	srv := env.Server()
	defer srv.Close()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &double.ContractTransport{
		Tenant: func() string { return env.TenantToken(seed.Org) },
		Vendor: func() http.Header {
			return env.VendorHeaders("admin.org.read", "admin.org.create", "admin.registration.verify")
		},
		Svc: func(string) http.Header { return env.ServiceHeaders("identity", seed.Org) },
	}}
	contracttest.Run(t, contracttest.Options{SpecPath: spec, BaseURL: srv.URL, Client: client, Skip: skip(spec)})
}

func TestContractExampleWorld(t *testing.T) {
	runPass(t, true, func(string) map[string]string { return contractenv.SkipExampleWorld() })
}

func TestContractOnboarding(t *testing.T) {
	runPass(t, false, func(spec string) map[string]string { return contractenv.SkipAllExcept(t, spec, "createOrganisation") })
}
