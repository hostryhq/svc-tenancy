package double_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// Callers test their resilience against the double: injected errors, delays and hangs, removable.
func TestFailureInjection(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	h := itest.Bearer(env.TenantToken(seed.Org))
	outlets := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/v1/outlets") }

	remove := env.Inject(double.Fault{Match: outlets, Status: 503})
	r := itest.Do(t, srv.URL, "GET", "/v1/outlets", h, nil)
	if r.Status != 503 || r.Code() != "injected" {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if got := itest.Do(t, srv.URL, "GET", "/v1/brands", h, nil); got.Status == 503 {
		t.Fatal("only matching requests fail")
	}
	remove()
	if got := itest.Do(t, srv.URL, "GET", "/v1/outlets", h, nil); got.Status == 503 {
		t.Fatal("a removed fault must stop applying")
	}

	env.Inject(double.Fault{Match: outlets, Delay: 150 * time.Millisecond})
	start := time.Now()
	itest.Do(t, srv.URL, "GET", "/v1/outlets", h, nil)
	if time.Since(start) < 150*time.Millisecond {
		t.Fatal("the injected delay did not apply")
	}

	env2, _ := double.NewEnv()
	srv2 := env2.Server()
	defer srv2.Close()
	env2.Inject(double.Fault{Match: func(*http.Request) bool { return true }, Hang: true})
	c := &http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Get(srv2.URL + "/v1/outlets"); err == nil {
		t.Fatal("a hung request must time out for the caller")
	}
}
