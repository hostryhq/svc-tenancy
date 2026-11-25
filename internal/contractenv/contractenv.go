// Package contractenv runs the shared contract suite (api-specs/contracttest) for this service. The double
// and the real service use the same entry point, so both are held to the same checks.
//
// The suite is example-driven: it sends each operation once with the spec's example inputs. Examples are
// true only in some state of the world, so before each operation the suite asks the target to be put into
// that operation's example world (x-example-worlds, see seed.ApplyWorld) and this package authenticates the
// request as the world's caller. No operation is skipped.
package contractenv

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hostryhq/api-specs/contracttest"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// SpecPath returns the tenancy OpenAPI document of the api-specs checkout: API_SPECS_DIR, or the module
// cache entry this repository pins.
func SpecPath(t testing.TB) string {
	t.Helper()
	root := os.Getenv("API_SPECS_DIR")
	if root == "" {
		out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/hostryhq/api-specs").Output()
		if err != nil {
			t.Fatalf("locate api-specs: %v", err)
		}
		root = strings.TrimSpace(string(out))
	}
	return filepath.Join(root, "openapi/tenancy/v1/openapi.yaml")
}

// Run executes the whole contract suite against base. seedWorld puts the target into an example world;
// tk mints the credentials the target trusts. It returns the worlds the suite asked for, in order.
func Run(t *testing.T, base string, tk *double.Tokens, seedWorld func(ctx context.Context, world string) error) []string {
	t.Helper()
	var mu sync.Mutex
	var current string
	var asked []string
	world := func() string { mu.Lock(); defer mu.Unlock(); return current }
	client := &http.Client{Timeout: 10 * time.Second, Transport: &double.ContractTransport{
		Tenant: func() string { return tk.TenantToken(seed.WorldCaller(world())) },
		Vendor: func() http.Header {
			return tk.VendorHeaders("admin.org.read", "admin.org.create", "admin.registration.verify")
		},
		Svc: func(path string) http.Header {
			if path == "/internal/v1/link-decisions" { // any registered service, acting for the invited party
				return tk.ServiceHeaders("orders", seed.Restaurants)
			}
			return tk.ServiceHeaders("identity", seed.Org)
		},
	}}
	contracttest.Run(t, contracttest.Options{SpecPath: SpecPath(t), BaseURL: base, Client: client,
		SeedWorld: func(ctx context.Context, w string) error {
			mu.Lock()
			current = w
			asked = append(asked, w)
			mu.Unlock()
			return seedWorld(ctx, w)
		}})
	return asked
}
