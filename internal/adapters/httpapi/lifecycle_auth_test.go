package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestS3LifecycleJWTOnlyAuthExactTargetAndOperation(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	v, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}
	fixture := func() map[string]any {
		return map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving"}, "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Unix() + 60, "jti": authUUID, "kind": "service", "sub": "service:auth", "azp": "umir-auth", "operation": "runtime.register", "permissions": []string{"runtime.register"}, "org_id": authOrg, "tenant_id": authTenant, "runtime_epoch": authEpoch, "lifecycle_version": "1"}
	}
	if _, err = v.Verify(context.Background(), signFixture(t, key, header, fixture())); err != nil {
		t.Fatalf("valid lifecycle token: %v", err)
	}
	for _, edit := range []func(map[string]any){func(c map[string]any) { c["sub"] = "service:gateway" }, func(c map[string]any) { c["azp"] = "umir-gateway" }, func(c map[string]any) { c["aud"] = []string{"umir-serving", "umir-warehouse"} }, func(c map[string]any) { c["operation"] = "expiry.due" }, func(c map[string]any) { c["permissions"] = []string{"runtime.close"} }, func(c map[string]any) { c["exp"] = at.Unix() }, func(c map[string]any) { delete(c, "iat") }, func(c map[string]any) { c["runtime_epoch"] = "bad" }, func(c map[string]any) { c["sid"] = authUUID }} {
		c := fixture()
		edit(c)
		if _, err = v.Verify(context.Background(), signFixture(t, key, header, c)); err == nil {
			t.Fatal("invalid lifecycle token accepted")
		}
	}
}
