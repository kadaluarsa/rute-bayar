package routetoken

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

func testSigner(t *testing.T, spec string) *LocalSigner {
	t.Helper()
	s, err := ParseKeys(spec)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(rune(b)), 32)))
}

func sampleClaims() Claims {
	return Claims{
		Environment: domain.EnvironmentSandbox,
		OrderID:     "ord_1",
		MandateID:   "mdt_1",
		MerchantID:  "mer_1",
		CartHash:    "sha256:abc",
		Amount:      150000,
		Currency:    "IDR",
		Method:      "va_mandiri",
		Providers:   []domain.ProviderCode{domain.ProviderMidtrans, domain.ProviderXendit},
	}
}

func TestIssueVerifyRoundTrip(t *testing.T) {
	issuer := NewIssuer(testSigner(t, "k1:"+key('a')), 15*time.Minute)
	token, claims, err := issuer.Issue(IssueInput{Claims: sampleClaims()})
	if err != nil {
		t.Fatal(err)
	}
	got, err := issuer.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != claims.ID || got.Amount != 150000 || !got.AllowsProvider(domain.ProviderXendit) || got.AllowsProvider(domain.ProviderDoku) {
		t.Fatalf("unexpected claims %+v", got)
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	issuer := NewIssuer(testSigner(t, "k1:"+key('a')), 15*time.Minute)
	token, _, err := issuer.Issue(IssueInput{Claims: sampleClaims()})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	tampered := sampleClaims()
	tampered.Amount = 1
	forged, _, _ := NewIssuer(testSigner(t, "k1:"+key('b')), time.Minute).Issue(IssueInput{Claims: tampered})
	forgedParts := strings.Split(forged, ".")
	// Swap in the forged payload but keep the original signature.
	parts[1] = forgedParts[1]
	if _, err := issuer.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrSignature) {
		t.Fatalf("expected signature error, got %v", err)
	}
	if _, err := issuer.Verify(forged); !errors.Is(err, ErrSignature) {
		t.Fatalf("token signed with another key must fail, got %v", err)
	}
	if _, err := issuer.Verify("garbage"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("expected malformed, got %v", err)
	}
}

func TestExpiryCappedByMandateAndRotation(t *testing.T) {
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.UTC)
	oldSigner := testSigner(t, "k1:"+key('a'))
	issuer := NewIssuer(oldSigner, 15*time.Minute)
	issuer.now = func() time.Time { return now }
	token, claims, err := issuer.Issue(IssueInput{Claims: sampleClaims(), NotAfter: now.Add(5 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if claims.ExpiresAt != now.Add(5*time.Minute).Unix() {
		t.Fatalf("expiry not capped by mandate: %d", claims.ExpiresAt)
	}

	// Rotation: k2 becomes active, k1 still verifies.
	rotated := NewIssuer(testSigner(t, "k2:"+key('c')+",k1:"+key('a')), 15*time.Minute)
	rotated.now = issuer.now
	if _, err := rotated.Verify(token); err != nil {
		t.Fatalf("old-key token should verify after rotation: %v", err)
	}

	issuer.now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := issuer.Verify(token); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected expired, got %v", err)
	}
	if _, _, err := issuer.Issue(IssueInput{Claims: sampleClaims(), NotAfter: now}); err == nil {
		t.Fatal("issuing past mandate expiry must fail")
	}
}

func TestIssueValidatesClaims(t *testing.T) {
	issuer := NewIssuer(testSigner(t, "k1:"+key('a')), time.Minute)
	c := sampleClaims()
	c.CartHash = ""
	if _, _, err := issuer.Issue(IssueInput{Claims: c}); err == nil || !strings.Contains(err.Error(), "cart_hash") {
		t.Fatalf("expected cart_hash error, got %v", err)
	}
	if _, err := ParseKeys("k1:" + base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("short keys must be rejected")
	}
}
