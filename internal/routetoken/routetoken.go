// Package routetoken issues and verifies Wishpull routing tokens
// (PRD v0.3 RP-2 and the anti-tampering NFR).
//
// A routing token binds one route decision to one order: order, mandate,
// merchant, cart hash, exact amount, method and the gateways the router may
// use. The execution layer refuses any charge that does not match the token
// byte for byte, so a quoted price cannot drift before payment.
//
// Format: "rbt1.<base64url(claims JSON)>.<kid>.<base64url(HMAC-SHA256)>".
// The token carries no credentials and is safe to show to an AI agent; it
// grants nothing on its own because execution also requires the user's
// mandate and an executor API key.
package routetoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pendig/rute-bayar/internal/domain"
)

const prefix = "rbt1"

// Claims are the facts a routing token pins.
type Claims struct {
	Version      int                   `json:"v"`
	ID           string                `json:"jti"`
	IssuedAt     int64                 `json:"iat"`
	ExpiresAt    int64                 `json:"exp"`
	Environment  domain.Environment    `json:"env"`
	OrderID      string                `json:"order_id"`
	MandateID    string                `json:"mandate_id"`
	MerchantID   string                `json:"merchant_id"`
	CartHash     string                `json:"cart_hash"`
	UserID       string                `json:"user_id,omitempty"`
	Amount       int64                 `json:"amount"`
	Currency     string                `json:"currency"`
	Method       string                `json:"method"`
	Providers    []domain.ProviderCode `json:"providers"`
	EstimatedFee int64                 `json:"estimated_fee"`
}

// AllowsProvider reports whether code is one of the pinned gateways.
func (c Claims) AllowsProvider(code domain.ProviderCode) bool {
	for _, p := range c.Providers {
		if p == code {
			return true
		}
	}
	return false
}

// Signer abstracts the MAC key. LocalSigner keeps keys in memory (loaded
// from Secret Manager through the environment); a Cloud KMS HMAC key can
// implement the same interface with MacSign/MacVerify.
type Signer interface {
	ActiveKeyID() string
	Sign(keyID string, message []byte) ([]byte, error)
	Verify(keyID string, message, mac []byte) (bool, error)
}

// LocalSigner holds one or more HMAC keys for rotation.
type LocalSigner struct {
	active string
	keys   map[string][]byte
}

// ParseKeys reads "kid1:base64key,kid2:base64key"; the first is active.
// Keys must be at least 32 bytes.
func ParseKeys(spec string) (*LocalSigner, error) {
	signer := &LocalSigner{keys: map[string][]byte{}}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kid, encoded, ok := strings.Cut(part, ":")
		kid = strings.TrimSpace(kid)
		if !ok || kid == "" || strings.ContainsAny(kid, ".") {
			return nil, fmt.Errorf("routing token key %q must look like kid:base64", part)
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil {
			key, err = base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
		}
		if err != nil {
			return nil, fmt.Errorf("routing token key %q is not base64: %w", kid, err)
		}
		if len(key) < 32 {
			return nil, fmt.Errorf("routing token key %q must be at least 32 bytes", kid)
		}
		if signer.active == "" {
			signer.active = kid
		}
		signer.keys[kid] = key
	}
	if signer.active == "" {
		return nil, errors.New("no routing token keys configured")
	}
	return signer, nil
}

func (s *LocalSigner) ActiveKeyID() string { return s.active }

func (s *LocalSigner) Sign(keyID string, message []byte) ([]byte, error) {
	key, ok := s.keys[keyID]
	if !ok {
		return nil, fmt.Errorf("unknown routing token key %q", keyID)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return mac.Sum(nil), nil
}

func (s *LocalSigner) Verify(keyID string, message, sig []byte) (bool, error) {
	expected, err := s.Sign(keyID, message)
	if err != nil {
		return false, err
	}
	return hmac.Equal(expected, sig), nil
}

// Issuer creates and verifies tokens.
type Issuer struct {
	signer Signer
	ttl    time.Duration
	now    func() time.Time
}

func NewIssuer(signer Signer, ttl time.Duration) *Issuer {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Issuer{signer: signer, ttl: ttl, now: time.Now}
}

// IssueInput carries the claims the caller controls.
type IssueInput struct {
	Claims
	// NotAfter caps the token lifetime, typically the mandate expiry.
	NotAfter time.Time
}

var (
	ErrMalformed = errors.New("routing token is malformed")
	ErrSignature = errors.New("routing token signature is invalid")
	ErrExpired   = errors.New("routing token has expired")
)

func (i *Issuer) Issue(input IssueInput) (string, Claims, error) {
	if i == nil || i.signer == nil {
		return "", Claims{}, errors.New("routing token issuer is not configured")
	}
	claims := input.Claims
	if err := validateClaims(claims); err != nil {
		return "", Claims{}, err
	}
	now := i.now().UTC()
	expires := now.Add(i.ttl)
	if !input.NotAfter.IsZero() && input.NotAfter.Before(expires) {
		expires = input.NotAfter.UTC()
	}
	if !expires.After(now) {
		return "", Claims{}, errors.New("routing token would already be expired (mandate expired?)")
	}
	claims.Version = 1
	claims.ID = newID()
	claims.IssuedAt = now.Unix()
	claims.ExpiresAt = expires.Unix()

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", Claims{}, fmt.Errorf("marshal routing token: %w", err)
	}
	kid := i.signer.ActiveKeyID()
	body := prefix + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + kid
	sig, err := i.signer.Sign(kid, []byte(body))
	if err != nil {
		return "", Claims{}, err
	}
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), claims, nil
}

func (i *Issuer) Verify(token string) (Claims, error) {
	if i == nil || i.signer == nil {
		return Claims{}, errors.New("routing token issuer is not configured")
	}
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 4 || parts[0] != prefix {
		return Claims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	body := strings.Join(parts[:3], ".")
	ok, err := i.signer.Verify(parts[2], []byte(body), sig)
	if err != nil || !ok {
		return Claims{}, ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Version != 1 {
		return Claims{}, ErrMalformed
	}
	if i.now().UTC().Unix() >= claims.ExpiresAt {
		return claims, ErrExpired
	}
	return claims, nil
}

func validateClaims(c Claims) error {
	missing := []string{}
	if strings.TrimSpace(c.OrderID) == "" {
		missing = append(missing, "order_id")
	}
	if strings.TrimSpace(c.MandateID) == "" {
		missing = append(missing, "mandate_id")
	}
	if strings.TrimSpace(c.MerchantID) == "" {
		missing = append(missing, "merchant_id")
	}
	if strings.TrimSpace(c.CartHash) == "" {
		missing = append(missing, "cart_hash")
	}
	if strings.TrimSpace(c.Method) == "" {
		missing = append(missing, "method")
	}
	if len(c.Providers) == 0 {
		missing = append(missing, "providers")
	}
	if len(missing) > 0 {
		return fmt.Errorf("routing token requires %s", strings.Join(missing, ", "))
	}
	if c.Amount <= 0 {
		return errors.New("routing token amount must be greater than zero")
	}
	if c.Currency != "IDR" {
		return fmt.Errorf("routing token currency must be IDR, got %q", c.Currency)
	}
	return nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return "rt_" + hex.EncodeToString(b[:])
}
