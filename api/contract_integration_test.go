//go:build integration

package api_test

import (
	"testing"

	"github.com/hostryhq/svc-tenancy/internal/contractenv"
	"github.com/hostryhq/svc-tenancy/internal/itest"
)

// TestRealServiceContract runs the same shared contract suite as the double against the real service on
// Postgres: every operation, each in its example world (the database is emptied and reseeded before each
// operation), nothing skipped.
func TestRealServiceContract(t *testing.T) {
	s := itest.New(t)
	contractenv.Run(t, s.Srv.URL, s.Tokens, s.SeedWorld)
}
