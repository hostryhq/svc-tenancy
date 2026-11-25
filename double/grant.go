package double

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"time"

	"github.com/hostryhq/go-auth"
	"github.com/hostryhq/go-auth/authtest"
	"github.com/hostryhq/go-common/clock"
)

// GrantIssuer is the issuer of the vendor approval grants the test tokens mint and the double accepts.
const GrantIssuer = "https://identity.hostry.invalid/vendor"

// GrantKid is the key id of the test grant signing key.
const GrantKid = "vendor-realm-1"

// ExampleApprover is the vendor user of the contract examples that approves lifecycle actions and records
// payments (recorded_by in the spec examples); ExampleRequester files the request.
const (
	ExampleApprover  = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a78"
	ExampleRequester = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a66"
)

// AllVendorPerms are the admin permissions tenancy's vendor operations use.
var AllVendorPerms = []string{"admin.org.read", "admin.org.create", "admin.registration.verify", "admin.subscription.read", "admin.subscription.start",
	"admin.subscription.manage", "admin.subscription.adjust", "admin.plan.manage", "admin.report.ledger", "admin.org.lifecycle.approve"}

// GrantKeySet is the public half of the test grant key, as identity's vendor-realm key set.
func (t *Tokens) GrantKeySet() auth.KeySet {
	return auth.NewStaticKeySet(auth.KeyDoc{Keys: map[string]*ecdsa.PublicKey{GrantKid: &t.GrantKey.PublicKey}})
}

// GrantVerifier returns a verifier for the test grants, judging expiry against clk. The service consumes the
// grant id itself, so Used is nil.
func (t *Tokens) GrantVerifier(clk clock.Clock) *auth.GrantVerifier {
	return &auth.GrantVerifier{Keys: t.GrantKeySet(), Issuer: GrantIssuer, Audience: "tenancy", Clock: clk}
}

// GrantSpec describes a vendor approval grant to mint. Zero fields take the defaults of the contract examples.
type GrantSpec struct {
	Kind        string // org_suspend, org_reinstate or org_close
	Org         string
	Target      string // the status the action sets; defaults from Kind
	ApprovalID  string
	JTI         string
	Requester   string
	Approver    string
	SubjectHash string // overrides the computed hash (to test a subject mismatch)
	Now         time.Time
	Lifetime    time.Duration // defaults to 300 seconds
}

var targetOf = map[string]string{"org_suspend": "suspended", "org_reinstate": "active", "org_close": "closed"}

// MintGrant signs a grant as identity would: ES256 compact JWS with the test key.
func (t *Tokens) MintGrant(g GrantSpec) string {
	if g.Target == "" {
		g.Target = targetOf[g.Kind]
	}
	if g.ApprovalID == "" {
		g.ApprovalID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a6d"
	}
	if g.JTI == "" {
		g.JTI = newJTI()
	}
	if g.Requester == "" {
		g.Requester = ExampleRequester
	}
	if g.Approver == "" {
		g.Approver = ExampleApprover
	}
	if g.SubjectHash == "" {
		g.SubjectHash = auth.SubjectHash(g.Kind, g.Org, g.Target)
	}
	if g.Now.IsZero() {
		g.Now = time.Now()
	}
	if g.Lifetime == 0 {
		g.Lifetime = 300 * time.Second
	}
	claims := auth.GrantClaims{Issuer: GrantIssuer, Realm: auth.RealmVendor, Audience: "tenancy", Type: auth.GrantType, JTI: g.JTI, ApprovalID: g.ApprovalID,
		Kind: g.Kind, Org: g.Org, Requester: g.Requester, Approver: g.Approver, SubjectHash: g.SubjectHash,
		IssuedAt: g.Now.Unix(), ExpiresAt: g.Now.Add(g.Lifetime).Unix()}
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": GrantKid})
	body, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hdr) + "." + enc.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, t.GrantKey, sum[:])
	if err != nil {
		panic(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig)
}

func newJTI() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	h := new(big.Int).SetBytes(b).Text(16)
	for len(h) < 32 {
		h = "0" + h
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// VendorHeadersAs is VendorHeaders for a named vendor user (the subject of the token).
func (t *Tokens) VendorHeadersAs(subject string, perms ...string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+t.VendorIssuer.Mint(authtest.Spec{Type: auth.TypeUser, Subject: subject, Grants: authtest.Grants(nil, perms...)}))
	tok, err := t.Registry.Signer("admin").Sign("tenancy", "")
	if err != nil {
		panic(err)
	}
	h.Set(auth.ServiceTokenHeader, tok)
	return h
}
