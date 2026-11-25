package double_test

import (
	"testing"
	"time"

	"github.com/hostryhq/svc-tenancy/double"
	"github.com/hostryhq/svc-tenancy/internal/contractenv"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// TestContract runs the shared contract suite (api-specs/contracttest) against the double: every
// operation, each in its example world, nothing skipped.
func TestContract(t *testing.T) {
	env, err := double.NewEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := env.Server()
	defer srv.Close()
	contractenv.Run(t, srv.URL, env.Tokens, env.SeedWorld, func() time.Time { return seed.At })
}
