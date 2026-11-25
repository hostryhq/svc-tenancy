//go:build integration

package pg_test

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/hostryhq/go-common/ids"

	"github.com/hostryhq/svc-tenancy/internal/itest"
	"github.com/hostryhq/svc-tenancy/internal/seed"
)

// ADR-0003 N1 R1: the stored idempotency record never holds the invitation code; a replay returns the
// same link without it.
func TestInvitationCodeIsNeverStoredAndAReplayWithholdsIt(t *testing.T) {
	s := itest.New(t)
	s.Seed(t, seed.Example, seed.Other, seed.Third)
	h := itest.Bearer(s.TenantToken(tenantA))
	h.Set("Idempotency-Key", "propose-once")
	body := obj{"id": ids.New().String(), "template": "supplier_buyer", "grants": []obj{{"scope": "test.echo.read", "direction": "outbound"}}}
	first := itest.Do(t, s.Srv.URL, "POST", "/v1/links", h, body)
	code, _ := first.JSON(t)["invitation_code"].(string)
	if first.Status != http.StatusCreated || len(code) != 16 {
		t.Fatalf("first: %d %s", first.Status, first.Body)
	}
	replay := itest.Do(t, s.Srv.URL, "POST", "/v1/links", h, body)
	if replay.Status != http.StatusCreated || strings.Contains(string(replay.Body), code) {
		t.Fatalf("replay must return the link without the code: %d %s", replay.Status, replay.Body)
	}
	if first.JSON(t)["link"].(map[string]any)["id"] != replay.JSON(t)["link"].(map[string]any)["id"] {
		t.Fatal("replay returned a different link")
	}
	var n int
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_records WHERE position($1 in convert_from(coalesce(body, ''), 'UTF8')) > 0`, code).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the code is stored in %d idempotency records", n)
	}
}

// N1 R2: the invitation capability covers only an unused, unexpired invitation; the proposer alone inserts
// invitations; the capability shows links and grants but no activity or amendments.
func TestInvitationCapabilityIsNarrow(t *testing.T) {
	s := stack(t)
	var linkID string
	if err := s.DB.Admin.QueryRowContext(ctx, `SELECT link_id FROM link_invitations WHERE proposer_tenant_id = $1`, tenantA.String()).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Admin.ExecContext(ctx, `INSERT INTO link_amendments (id, link_id, proposed_by_side, grants, status, created_at)
		VALUES ($1, $2, 'proposer', '[]', 'pending', now())`, ids.New(), linkID); err != nil {
		t.Fatal(err)
	}
	digest := hexOf(t, s.DB.Admin, linkID)

	seen := func(hash string, tbl string) int {
		n := 0
		inTx(t, s.DB.App, tenantC, func(tx *sql.Tx) {
			if hash != "" {
				if _, err := tx.ExecContext(ctx, `SELECT set_config('app.invite_hash', $1, true)`, hash); err != nil {
					t.Fatal(err)
				}
			}
			n = count(t, tx, `SELECT count(*) FROM `+tbl)
		})
		return n
	}

	t.Run("an unused, unexpired code shows its invitation, link and grants, and nothing else", func(t *testing.T) {
		for tbl, want := range map[string]bool{"link_invitations": true, "links": true, "link_grants": true, "link_activity": false, "link_amendments": false} {
			if got := seen(digest, tbl) > 0; got != want {
				t.Errorf("%s visible = %v, want %v", tbl, got, want)
			}
		}
	})
	t.Run("a wrong digest shows nothing", func(t *testing.T) {
		wrong := strings.Repeat("ab", 32)
		for _, tbl := range []string{"link_invitations", "links", "link_grants"} {
			if n := seen(wrong, tbl); n != 0 {
				t.Errorf("%s: %d rows", tbl, n)
			}
		}
	})
	t.Run("the digest holder cannot insert an invitation, not even for itself", func(t *testing.T) {
		for _, proposer := range []ids.ID{tenantC, tenantA} {
			inTx(t, s.DB.App, tenantC, func(tx *sql.Tx) {
				if _, err := tx.ExecContext(ctx, `SELECT set_config('app.invite_hash', $1, true)`, digest); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO link_invitations (code_hash, link_id, proposer_tenant_id, expires_at)
					VALUES (decode(repeat('cd', 32), 'hex'), $1, $2, now() + interval '1 day')`, linkID, proposer); err == nil {
					t.Errorf("C inserted an invitation with proposer %s", proposer)
				}
			})
		}
	})
	t.Run("an expired code shows nothing", func(t *testing.T) {
		if _, err := s.DB.Admin.ExecContext(ctx, `UPDATE link_invitations SET expires_at = now() - interval '1 second' WHERE link_id = $1`, linkID); err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"link_invitations", "links", "link_grants"} {
			if n := seen(digest, tbl); n != 0 {
				t.Errorf("%s: %d rows", tbl, n)
			}
		}
		if _, err := s.DB.Admin.ExecContext(ctx, `UPDATE link_invitations SET expires_at = now() + interval '1 day' WHERE link_id = $1`, linkID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a used code shows nothing to anyone but the proposer and the organisation that used it", func(t *testing.T) {
		if _, err := s.DB.Admin.ExecContext(ctx, `UPDATE link_invitations SET used_at = now(), used_by = $2 WHERE link_id = $1`, linkID, tenantB.String()); err != nil {
			t.Fatal(err)
		}
		for _, tbl := range []string{"link_invitations", "links", "link_grants"} {
			if n := seen(digest, tbl); n != 0 {
				t.Errorf("%s: %d rows", tbl, n)
			}
		}
	})
}

func hexOf(t *testing.T, db *sql.DB, linkID string) string {
	t.Helper()
	var h string
	if err := db.QueryRowContext(ctx, `SELECT encode(code_hash, 'hex') FROM link_invitations WHERE link_id = $1`, linkID).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}
