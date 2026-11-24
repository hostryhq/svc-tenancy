// Package double is the in-memory test double of the tenancy service. It runs the real API layer and
// service over an in-memory repository, so it implements the whole public contract, including the
// uniqueness rules, idempotency, outlet scoping and vendor audit, with deterministic ids and time. Other
// services' tests import it instead of the real service. It is verified by the shared contract suite
// (api-specs/contracttest) so it cannot drift.
//
// Authentication uses the same go-auth verifiers as the real service: give it the test issuers of
// go-auth/authtest. Failure injection (error status, delay, hang) lets callers test their resilience.
package double

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/events"
	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/api"
	"github.com/hostryhq/svc-tenancy/internal/gen"
	"github.com/hostryhq/svc-tenancy/internal/seed"
	"github.com/hostryhq/svc-tenancy/repository/mem"
	"github.com/hostryhq/svc-tenancy/service"
)

// Config configures the double. Tenant, Vendor and Services are required; the rest has defaults.
type Config struct {
	Tenant   *auth.Verifier        // tenant-realm tokens (authtest.NewTenantIssuer(...).Verifier())
	Vendor   *auth.Verifier        // vendor-realm tokens
	Services *auth.ServiceVerifier // service tokens (self must be "tenancy")
	// Clock defaults to a fixed instant, the instant of the spec examples. Entity timestamps use it;
	// token checks use the verifiers' own clocks.
	Clock clock.Clock
	// IDs defaults to a generator driven by Clock with a counter, so ids are deterministic.
	IDs *ids.Generator
	// LinksEnabled registers the link routes (default true in the double).
	LinksOff bool
	// AnyService stands for "*" in x-service-callers.
	AnyService []string
}

// Fault is an injected failure for matching requests.
type Fault struct {
	Match  func(*http.Request) bool
	Status int           // respond with this status and a problem body (0: pass through)
	Delay  time.Duration // sleep before answering
	Hang   bool          // never answer until the request is cancelled (a timeout for the caller)
}

// Double is the running double.
type Double struct {
	Store *mem.Store
	Svc   *service.Service
	h     http.Handler
	mu    sync.Mutex
	fault []Fault
}

// New builds a double. The store is empty; call SeedExampleWorld to match the spec examples.
func New(cfg Config) (*Double, error) {
	cat, err := gen.Catalogue()
	if err != nil {
		return nil, err
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Func(func() time.Time { return seed.At })
	}
	if cfg.IDs == nil {
		cfg.IDs = ids.NewGenerator(cfg.Clock.Now, &counter{})
	}
	if len(cfg.AnyService) == 0 {
		cfg.AnyService = []string{"identity", "admin", "sync", "billing", "orders", "hospitality", "notifications"}
	}
	store := mem.New(cfg.IDs)
	svc := service.New(store, service.Options{Clock: cfg.Clock, IDs: cfg.IDs, Log: slog.New(slog.DiscardHandler)})
	guard := &auth.Guard{Catalogue: cat, Tenant: cfg.Tenant, Vendor: cfg.Vendor, Services: cfg.Services, Audit: store,
		Approvals: &auth.ApprovalChecker{Verifier: cfg.Tenant, Used: auth.NewMemorySingleUse(nil)}, Log: slog.New(slog.DiscardHandler)}
	d := &Double{Store: store, Svc: svc}
	d.h = api.New(api.Config{Service: svc, Guard: guard, Clock: cfg.Clock, Log: slog.New(slog.DiscardHandler), LinksEnabled: !cfg.LinksOff, AnyService: cfg.AnyService})
	return d, nil
}

type counter struct{ n byte }

func (c *counter) Read(p []byte) (int, error) {
	for i := range p {
		c.n++
		p[i] = c.n
	}
	return len(p), nil
}

// SeedExampleWorld stores the organisation the spec examples refer to (see internal/seed).
func (d *Double) SeedExampleWorld(ctx context.Context) error { return seed.ExampleWorld(ctx, d.Store) }

// Events returns the events the double published to its outbox.
func (d *Double) Events() []events.Envelope { return d.Store.Events() }

// Inject adds a failure for matching requests; it returns a function that removes it.
func (d *Double) Inject(f Fault) (remove func()) {
	d.mu.Lock()
	d.fault = append(d.fault, f)
	i := len(d.fault) - 1
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.fault[i].Match = nil
		d.mu.Unlock()
	}
}

// ServeHTTP implements http.Handler.
func (d *Double) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	var hit *Fault
	for i := range d.fault {
		if d.fault[i].Match != nil && d.fault[i].Match(r) {
			f := d.fault[i]
			hit = &f
			break
		}
	}
	d.mu.Unlock()
	if hit != nil {
		if hit.Hang {
			<-r.Context().Done()
			return
		}
		if hit.Delay > 0 {
			select {
			case <-time.After(hit.Delay):
			case <-r.Context().Done():
				return
			}
		}
		if hit.Status != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(hit.Status)
			_, _ = w.Write([]byte(`{"type":"urn:hostry:problem:injected","title":"Injected failure","status":` + itoa(hit.Status) + `,"code":"injected"}`))
			return
		}
	}
	d.h.ServeHTTP(w, r)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for ; n > 0; n /= 10 {
		i--
		b[i] = byte('0' + n%10)
	}
	return string(b[i:])
}

// Server starts the double on an httptest server.
func (d *Double) Server() *httptest.Server { return httptest.NewServer(d) }
