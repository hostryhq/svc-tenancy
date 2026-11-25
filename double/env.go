package double

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/clock"
	"github.com/hostryhq/go-common/ids"
)

// Tokens are the test issuers a service under test trusts, with helpers that mint the credentials each
// kind of request needs. For tests only: tokens are signed with throw-away keys.
type Tokens struct {
	TenantIssuer *authtest.Issuer
	VendorIssuer *authtest.Issuer
	Registry     *authtest.ServiceRegistry
	// GrantKey signs the vendor approval grants of tests (the role of identity's vendor-realm key).
	GrantKey *ecdsa.PrivateKey
}

// NewTokens creates issuers and a service registry containing identity, admin, tenancy and the given services.
func NewTokens(extraServices ...string) *Tokens {
	clk := clock.System
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Tokens{TenantIssuer: authtest.NewTenantIssuer(clk), VendorIssuer: authtest.NewVendorIssuer(clk), GrantKey: key,
		Registry: authtest.NewServiceRegistry(clk, append([]string{"identity", "admin", "tenancy", "billing", "sync", "orders"}, extraServices...)...)}
}

// Env is a double together with the tokens its verifiers trust.
type Env struct {
	*Double
	*Tokens
}

// NewEnv builds a double that trusts fresh test issuers and a service registry containing the given
// services (identity and admin are always present).
func NewEnv(extraServices ...string) (*Env, error) { return NewEnvLinks(true, extraServices...) }

// NewEnvLinks is NewEnv with the link routes switched on or off (LINKS_ENABLED).
func NewEnvLinks(links bool, extraServices ...string) (*Env, error) {
	tk := NewTokens(extraServices...)
	d, err := New(Config{Tenant: tk.TenantIssuer.Verifier(), Vendor: tk.VendorIssuer.Verifier(), Services: tk.Registry.Verifier("tenancy"), LinksOff: !links,
		GrantKeys: tk.GrantKeySet()})
	if err != nil {
		return nil, err
	}
	return &Env{Double: d, Tokens: tk}, nil
}

// TenantToken mints a strong (OTP, fresh) user token for tenant with the given grants. Without grants it
// is an owner: every permission at every outlet.
func (e *Tokens) TenantToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Tenant: tenant, Subject: "member-1", Grants: grants})
}

// PinToken mints a shared-device PIN session token, which can never exercise strong-only permissions.
func (e *Tokens) PinToken(tenant ids.ID, grants ...auth.Grant) string {
	if len(grants) == 0 {
		grants = authtest.OwnerGrants()
	}
	return e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypePin, Tenant: tenant, Grants: grants})
}

// VendorHeaders returns the two credentials a vendor operation needs: the vendor user's token with the
// given admin permissions and the admin service token.
func (e *Tokens) VendorHeaders(perms ...string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+e.VendorIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Subject: "vendor-user-1", Grants: authtest.Grants(nil, perms...)}))
	tok, err := e.Registry.Signer("admin").Sign("tenancy", "")
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}

// SupportHeaders returns a support-session token (view mode) for tenant plus the admin service token.
func (e *Tokens) SupportHeaders(tenant ids.ID) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+e.TenantIssuer.Mint(authtest.Spec{Type: auth.TypeSupport, Tenant: tenant, Grants: authtest.Grants(nil, "organisation.read", "brand.read", "outlet.read", "tax_registration.read", "settings.read", "series.read", "organisation.manage", "brand.manage")}))
	tok, err := e.Registry.Signer("admin").Sign("tenancy", tenant.String())
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}

// ServiceHeaders returns the service token a service (for example identity) sends, naming tenant.
func (e *Tokens) ServiceHeaders(service string, tenant ids.ID) http.Header {
	h := http.Header{}
	tok, err := e.Registry.Signer(service).Sign("tenancy", tenant.String())
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}

// ContractTransport authenticates each request of the contract suite for the example world: tenant
// operations as the example organisation's owner, vendor operations as an onboarding vendor user through
// the admin service, and internal operations as identity.
type ContractTransport struct {
	Base   http.RoundTripper
	Tenant func() string
	Vendor func() http.Header
	Svc    func(path string) http.Header
	// Grant, when set, returns the Hostry-Vendor-Approval grant for a vendor request ("" for none): the example
	// grant of the spec is for one kind, one clock and one jti, so the suite gets a fresh grant per request.
	Grant func(method, path string) string
}

// RoundTrip implements http.RoundTripper.
func (t *ContractTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	var h http.Header
	switch {
	case hasPrefix(r.URL.Path, "/internal/v1/vendor/"):
		h = t.Vendor()
		if t.Grant != nil {
			if g := t.Grant(r.Method, r.URL.Path); g != "" {
				r.Header.Set("Hostry-Vendor-Approval", g)
			}
		}
	case hasPrefix(r.URL.Path, "/internal/v1/"):
		h = t.Svc(r.URL.Path)
	default:
		r.Header.Set("Authorization", "Bearer "+t.Tenant())
	}
	for k, vs := range h {
		r.Header.Del(k)
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
