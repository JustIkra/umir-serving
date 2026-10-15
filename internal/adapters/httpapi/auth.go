package httpapi

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpjson"
)

type TokenVerifier interface {
	Verify(context.Context, string) (contract.RequestScope, error)
}
type SigningKeySource func(context.Context, string) (ed25519.PublicKey, error)
type JWTOptions struct {
	Now  func() time.Time
	Keys SigningKeySource
}
type JWTVerifier struct{ options JWTOptions }

func NewJWTVerifier(options JWTOptions) (*JWTVerifier, error) {
	if options.Now == nil || options.Keys == nil {
		return nil, contract.ErrInvalidArgument
	}
	return &JWTVerifier{options: options}, nil
}

type jwtHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}
type userClaims struct {
	Issuer            string   `json:"iss"`
	Audience          []string `json:"aud"`
	IssuedAt          int64    `json:"iat"`
	NotBefore         int64    `json:"nbf"`
	Expires           int64    `json:"exp"`
	TokenID           string   `json:"jti"`
	Operation         string   `json:"operation"`
	Permissions       []string `json:"permissions"`
	Kind              string   `json:"kind"`
	Subject           string   `json:"sub"`
	SessionID         string   `json:"sid"`
	SessionVersion    string   `json:"session_version"`
	CredentialVersion string   `json:"credential_version"`
	AccessVersion     string   `json:"access_version"`
	OrgID             string   `json:"org_id"`
	TenantID          string   `json:"tenant_id"`
	TimeZone          string   `json:"time_zone"`
	SettingsVersion   string   `json:"settings_version"`
	LifecycleVersion  string   `json:"lifecycle_version"`
	AuthorizedParty   string   `json:"azp"`
}

func authFailure(code string, cause error) error {
	return &contract.OperationError{Code: code, Message: "request authorization failed", Outcome: "unavailable", Cause: cause}
}

func (v *JWTVerifier) Verify(ctx context.Context, token string) (contract.RequestScope, error) {
	invalid := authFailure("InvalidToken", nil)
	if len(token) == 0 || len(token) > 16384 {
		return contract.RequestScope{}, invalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return contract.RequestScope{}, invalid
	}
	var header jwtHeader
	bytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || httpjson.DecodeStrict(bytes, &header) != nil || header.Algorithm != "EdDSA" || header.Type != "JWT" || header.KeyID == "" || len(header.KeyID) > 256 {
		return contract.RequestScope{}, invalid
	}
	key, err := v.options.Keys(ctx, header.KeyID)
	if err != nil {
		return contract.RequestScope{}, authFailure("AuthUnavailable", err)
	}
	if len(key) != ed25519.PublicKeySize {
		return contract.RequestScope{}, invalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return contract.RequestScope{}, invalid
	}
	var claims userClaims
	bytes, err = base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return contract.RequestScope{}, invalid
	}
	var kind struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(bytes, &kind) != nil {
		return contract.RequestScope{}, invalid
	}
	if kind.Kind == "service" {
		return v.verifyLifecycle(bytes, token)
	}
	if httpjson.DecodeStrict(bytes, &claims) != nil {
		return contract.RequestScope{}, invalid
	}
	var required map[string]json.RawMessage
	if err = json.Unmarshal(bytes, &required); err != nil {
		return contract.RequestScope{}, invalid
	}
	for _, name := range []string{"iat", "nbf", "exp"} {
		if _, ok := required[name]; !ok {
			return contract.RequestScope{}, invalid
		}
	}
	now := v.options.Now()
	if now.IsZero() || claims.IssuedAt < 0 || claims.IssuedAt > now.Unix() || claims.NotBefore != claims.IssuedAt || claims.Expires <= claims.IssuedAt || claims.Expires-claims.IssuedAt != 60 || now.Unix() >= claims.Expires {
		return contract.RequestScope{}, invalid
	}
	if claims.Issuer != "urn:umir:auth" || claims.Kind != "user" || claims.AuthorizedParty != "umir-gateway" || claims.Operation == "" || !uniqueAllowed(claims.Audience, []string{"umir-warehouse", "umir-serving", "umir-gateway", "umir-feedback"}, 2) || (!slices.Contains(claims.Audience, "umir-serving") || !slices.Contains(claims.Audience, "umir-warehouse")) || !uniqueAllowed(claims.Permissions, capabilities, 40) {
		return contract.RequestScope{}, invalid
	}
	for _, id := range []string{claims.TokenID, claims.Subject, claims.SessionID, claims.OrgID, claims.TenantID} {
		if !contract.ValidEntityID(id) {
			return contract.RequestScope{}, invalid
		}
	}
	for _, version := range []string{claims.SessionVersion, claims.CredentialVersion, claims.AccessVersion, claims.SettingsVersion, claims.LifecycleVersion} {
		if !contract.ValidVersion(version) {
			return contract.RequestScope{}, invalid
		}
	}
	if claims.TimeZone == "" || len(claims.TimeZone) > 128 || claims.TimeZone == "Local" {
		return contract.RequestScope{}, invalid
	}
	if _, err = time.LoadLocation(claims.TimeZone); err != nil {
		return contract.RequestScope{}, invalid
	}
	scope := contract.RequestScope{Kind: claims.Kind, Subject: claims.Subject, OrgID: claims.OrgID, TenantID: claims.TenantID, UserID: claims.Subject, Operation: claims.Operation, Permissions: slices.Clone(claims.Permissions), TimeZone: claims.TimeZone, SettingsVersion: claims.SettingsVersion, LifecycleVersion: claims.LifecycleVersion}
	return scope.WithToken(token), nil
}

var capabilities = []string{"catalog.read", "catalog.write", "stock.read", "delivery.create", "analytics.read", "writeoff.confirm", "menu.read", "recommendations.read", "order.prepare", "order.confirm", "order.read", "feedback.submit", "org.read", "org.settings.write", "restaurant.create", "restaurant.delete", "org.delete", "membership.manage", "invitation.manage", "org.summary.read", "runtime.register", "runtime.close", "expiry.due", "consumer.menu.evaluate", "consumer.order.evaluate", "consumer.order.commit", "consumer.order.resolve", "feedback.anonymous.submit", "feedback.read", "feedback.export", "consumer.recommendations.read"}

func uniqueAllowed(values, allowed []string, max int) bool {
	if len(values) == 0 || len(values) > max {
		return false
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] || !slices.Contains(allowed, value) {
			return false
		}
		seen[value] = true
	}
	return true
}

type lifecycleClaims struct {
	Issuer           string   `json:"iss"`
	Audience         []string `json:"aud"`
	IssuedAt         *int64   `json:"iat"`
	NotBefore        *int64   `json:"nbf"`
	Expires          *int64   `json:"exp"`
	TokenID          string   `json:"jti"`
	Kind             string   `json:"kind"`
	Subject          string   `json:"sub"`
	AuthorizedParty  string   `json:"azp"`
	Operation        string   `json:"operation"`
	Permissions      []string `json:"permissions"`
	OrgID            string   `json:"org_id"`
	TenantID         string   `json:"tenant_id"`
	RuntimeEpoch     string   `json:"runtime_epoch"`
	LifecycleVersion string   `json:"lifecycle_version"`
}

func (v *JWTVerifier) verifyLifecycle(data []byte, token string) (contract.RequestScope, error) {
	invalid := authFailure("InvalidToken", nil)
	var c lifecycleClaims
	if httpjson.DecodeStrict(data, &c) != nil || c.IssuedAt == nil || c.NotBefore == nil || c.Expires == nil {
		return contract.RequestScope{}, invalid
	}
	now := v.options.Now()
	if now.IsZero() || *c.IssuedAt < 0 || *c.IssuedAt > now.Unix() || *c.NotBefore != *c.IssuedAt || *c.Expires <= *c.IssuedAt || *c.Expires-*c.IssuedAt != 60 || now.Unix() >= *c.Expires {
		return contract.RequestScope{}, invalid
	}
	if c.Issuer != "urn:umir:auth" || c.Kind != "service" || c.Subject != "service:auth" || c.AuthorizedParty != "umir-auth" || !slices.Equal(c.Audience, []string{"umir-serving"}) || (c.Operation != "runtime.register" && c.Operation != "runtime.close") || !slices.Equal(c.Permissions, []string{c.Operation}) || !contract.ValidVersion(c.LifecycleVersion) {
		return contract.RequestScope{}, invalid
	}
	for _, id := range []string{c.TokenID, c.OrgID, c.TenantID, c.RuntimeEpoch} {
		if !contract.ValidEntityID(id) {
			return contract.RequestScope{}, invalid
		}
	}
	return contract.RequestScope{Kind: c.Kind, Subject: c.Subject, OrgID: c.OrgID, TenantID: c.TenantID, RuntimeEpoch: c.RuntimeEpoch, Operation: c.Operation, Permissions: slices.Clone(c.Permissions), LifecycleVersion: c.LifecycleVersion}.WithToken(token), nil
}
