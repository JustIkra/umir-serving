package httpapi

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/contract"
)

const authTenant = "33333333-3333-4333-8333-333333333333"
const authOrg = "22222222-2222-4222-8222-222222222222"
const authUUID = "11111111-1111-4111-8111-111111111111"
const authEpoch = "44444444-4444-4444-8444-444444444444"

func claimsFixture(at time.Time) map[string]any {
	return map[string]any{"iss": "urn:umir:auth", "aud": []string{"umir-serving", "umir-warehouse"}, "iat": at.Unix(), "nbf": at.Unix(), "exp": at.Unix() + 60, "jti": authUUID, "operation": "menu", "permissions": []string{"menu.read", "consumer.menu.evaluate"}, "kind": "user", "sub": authUUID, "sid": authUUID, "session_version": "1", "credential_version": "1", "access_version": "1", "org_id": authOrg, "tenant_id": authTenant, "time_zone": "Europe/Moscow", "settings_version": "1", "lifecycle_version": "1", "azp": "umir-gateway"}
}

func signFixture(t *testing.T, key ed25519.PrivateKey, header, claims any) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	p, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(input)))
}

func TestS1JWTVerifierRejectsInvalidClaimsAndSignature(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	header := map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test-key"}
	token := signFixture(t, key, header, claimsFixture(at))
	scope, err := verifier.Verify(context.Background(), token)
	if err != nil || scope.TenantID != authTenant {
		t.Fatalf("valid token: %+v %v", scope, err)
	}
	for _, test := range []struct {
		name, field string
		value       any
	}{
		{"issuer", "iss", "forged"}, {"audience", "aud", []string{"umir-warehouse"}}, {"expired", "exp", at.Unix()}, {"future", "iat", at.Unix() + 1}, {"nbf", "nbf", at.Unix() - 1}, {"ttl", "exp", at.Unix() + 61}, {"subject", "sub", "service:auth"}, {"kind", "kind", "anonymous"}, {"azp", "azp", "umir-serving"}, {"missing", "sid", nil}, {"version", "access_version", "01"}, {"unknown", "roles", []string{"Admin"}}, {"zone", "time_zone", "Invalid/Zone"}, {"duplicate permissions", "permissions", []string{"menu.read", "menu.read"}}, {"unknown permission", "permissions", []string{"*"}}, {"duplicate audience", "aud", []string{"umir-serving", "umir-serving"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := claimsFixture(at)
			if test.value == nil {
				delete(claims, test.field)
			} else {
				claims[test.field] = test.value
			}
			if _, verifyErr := verifier.Verify(context.Background(), signFixture(t, key, header, claims)); verifyErr == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	for _, field := range []string{"alg", "typ", "jku", "jwk", "x5u"} {
		h := map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test-key"}
		h[field] = "bad"
		if _, verifyErr := verifier.Verify(context.Background(), signFixture(t, key, h, claimsFixture(at))); verifyErr == nil {
			t.Fatalf("invalid header %s accepted", field)
		}
	}
	_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(context.Background(), signFixture(t, wrongKey, header, claimsFixture(at))); err == nil {
		t.Fatal("forged signature accepted")
	}
	verifier, err = NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return nil, errors.New("keys unavailable") }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifier.Verify(context.Background(), token); err == nil {
		t.Fatal("unavailable keys accepted")
	}
}

type scopeResolverProbe struct {
	bound   bool
	calls   int
	scopes  []contract.RequestScope
	runtime Runtime
}

func (p *scopeResolverProbe) Resolve(ctx context.Context, s contract.RequestScope) (Runtime, contract.RuntimeInfo, error) {
	p.calls++
	bound, ok := contract.RequestScopeFromContext(ctx)
	p.bound = ok && bound.TenantID == s.TenantID && bound.Operation == s.Operation
	p.scopes = append(p.scopes, s)
	return p.runtime, contract.RuntimeInfo{Epoch: authEpoch, WarehouseEpoch: authEpoch}, nil
}

func TestS1EveryScopedRouteAuthorizesBeforeResolution(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	probe := &scopeResolverProbe{}
	handler, err := NewScopedHandler(probe, verifier, ScopedOptions{Options: Options{RequestTimeout: time.Second, MaxRequestBytes: 1024}, GatewayKey: "gateway-test", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct{ method, path, operation, permission string }{
		{"GET", "products", "servingProducts", "menu.read"}, {"GET", "dish-catalog", "servingDishes", "menu.read"}, {"GET", "dish?dishID=soup", "servingDish", "menu.read"}, {"GET", "menu", "menu", "menu.read"}, {"GET", "stop-list", "stopList", "menu.read"}, {"GET", "recommendations", "recommendations", "recommendations.read"}, {"POST", "order-proposals", "prepareOrder", "order.prepare"}, {"POST", "order-confirmations", "confirmOrder", "order.confirm"}, {"GET", "order-confirmations/resolve?commandID=c&proposalID=p&runtimeEpoch=" + authEpoch, "resolveOrder", "order.read"},
	}
	for _, route := range routes {
		t.Run(route.path, func(t *testing.T) {
			for _, kind := range []string{"missing-key", "missing-token", "bad-token", "foreign-tenant", "missing-permission", "wrong-operation", "identity-header", "duplicate-key"} {
				claims := claimsFixture(at)
				claims["operation"] = route.operation
				claims["permissions"] = []string{route.permission}
				request := httptest.NewRequest(route.method, "/internal/serving/v2/r/"+authTenant+"/"+route.path, strings.NewReader(`{}`))
				request.Header.Set("X-Umir-Service-Key", "gateway-test")
				request.Header.Set("X-Umir-Runtime-Epoch", authEpoch)
				switch kind {
				case "foreign-tenant":
					claims["tenant_id"] = authUUID
				case "missing-permission":
					claims["permissions"] = []string{"feedback.submit"}
				case "wrong-operation":
					claims["operation"] = "other"
				}
				request.Header.Set("Authorization", "Bearer "+signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims))
				switch kind {
				case "missing-key":
					request.Header.Del("X-Umir-Service-Key")
				case "missing-token":
					request.Header.Del("Authorization")
				case "bad-token":
					request.Header.Set("Authorization", "Bearer invalid")
				case "identity-header":
					request.Header.Set("X-Umir-User-ID", authUUID)
				case "duplicate-key":
					request.Header.Add("X-Umir-Service-Key", "gateway-test")
				}
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code < 400 || probe.calls != 0 {
					t.Fatalf("%s: status=%d calls=%d body=%s", kind, response.Code, probe.calls, response.Body.String())
				}
			}
		})
	}
}

type authorizedRuntimeStub struct {
	omitObservation bool
	Runtime
	at   time.Time
	seen []contract.RequestScope
}

func (s *authorizedRuntimeStub) capture(ctx context.Context) {
	scope, ok := contract.RequestScopeFromContext(ctx)
	if ok {
		s.seen = append(s.seen, scope)
		if !s.omitObservation {
			contract.RecordWarehouseContext(ctx, contract.WarehouseContext{OrgID: scope.OrgID, TenantID: scope.TenantID, Epoch: authEpoch, TimeZone: scope.TimeZone, SettingsVersion: scope.SettingsVersion, EvaluatedAt: s.at, StateVersion: 9007199254740993})
		}
	}
}
func (s *authorizedRuntimeStub) GetProducts(ctx context.Context) (contract.Products, error) {
	s.capture(ctx)
	return contract.Products{EvaluatedAt: s.at, StateVersion: 9007199254740993, Items: []contract.Product{}}, nil
}
func (s *authorizedRuntimeStub) GetDishCatalog(ctx context.Context) (contract.DishCatalog, error) {
	s.capture(ctx)
	return contract.DishCatalog{EvaluatedAt: s.at, Items: []contract.Dish{}}, nil
}
func (s *authorizedRuntimeStub) GetDish(ctx context.Context, id string) (contract.DishDetails, error) {
	s.capture(ctx)
	return contract.DishDetails{EvaluatedAt: s.at, Dish: contract.Dish{DishID: id, Recipe: []contract.Ingredient{}}}, nil
}
func (s *authorizedRuntimeStub) GetMenu(ctx context.Context) (contract.Menu, error) {
	s.capture(ctx)
	return contract.Menu{EvaluatedAt: s.at, Dishes: []contract.MenuDish{}}, nil
}
func (s *authorizedRuntimeStub) GetStopList(ctx context.Context) (contract.StopList, error) {
	s.capture(ctx)
	return contract.StopList{EvaluatedAt: s.at, Dishes: []contract.StopListDish{}}, nil
}
func (s *authorizedRuntimeStub) GetRecommendations(ctx context.Context) (contract.Recommendations, error) {
	s.capture(ctx)
	return contract.Recommendations{EvaluatedAt: s.at, Items: []contract.Recommendation{}}, nil
}
func (s *authorizedRuntimeStub) PrepareOrder(ctx context.Context, _ contract.PrepareOrderRequest) (contract.OrderProposal, error) {
	s.capture(ctx)
	return contract.OrderProposal{ProposalID: "p", EvaluatedAt: s.at, RequestedPositions: []contract.Position{}, AcceptedPositions: []contract.AcceptedPosition{}, Changes: []contract.Change{}}, nil
}
func (s *authorizedRuntimeStub) ConfirmOrder(ctx context.Context, _ contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	s.capture(ctx)
	return contract.ConfirmResult{Kind: contract.ConfirmationPending}, nil
}
func (s *authorizedRuntimeStub) ResolveConfirmation(ctx context.Context, _ contract.ConfirmOrderRequest) (contract.ConfirmResult, error) {
	s.capture(ctx)
	return contract.ConfirmResult{Kind: contract.ConfirmationPending}, nil
}

func TestS1AuthorizedRoutesBindScopeAndPreserveExactResults(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &authorizedRuntimeStub{at: at}
	probe := &scopeResolverProbe{runtime: runtime}
	handler, err := NewScopedHandler(probe, verifier, ScopedOptions{Options: Options{RequestTimeout: time.Second, MaxRequestBytes: 1024}, GatewayKey: "gateway-test", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range scopedRoutes {
		claims := claimsFixture(at)
		claims["operation"] = route.operation
		claims["permissions"] = []string{route.permission, "consumer.menu.evaluate", "consumer.order.evaluate", "consumer.order.commit", "consumer.order.resolve"}
		path := route.path
		body := `{"commandID":"c","proposalID":"p"}`
		if route.path == "dish" {
			path += "?dishID=soup"
		}
		if route.operation == "resolveOrder" {
			path += "?commandID=c&proposalID=p&runtimeEpoch=" + authEpoch
		}
		if route.operation == "prepareOrder" {
			body = `{"positions":[]}`
		}
		request := httptest.NewRequest(route.method, "/internal/serving/v2/r/"+authTenant+"/"+path, strings.NewReader(body))
		request.Header.Set("X-Umir-Service-Key", "gateway-test")
		request.Header.Set("X-Umir-Runtime-Epoch", authEpoch)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 && response.Code != 202 {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		if response.Header().Get("Cache-Control") != "no-store" || !strings.Contains(response.Body.String(), `"tenantID":"`+authTenant+`"`) || !strings.Contains(response.Body.String(), `"warehouseEpoch":"`+authEpoch+`"`) {
			t.Fatalf("missing scoped metadata: %s", response.Body.String())
		}
		if route.path == "products" && !strings.Contains(response.Body.String(), `"stateVersion":"9007199254740993"`) {
			t.Fatal("exact version lost")
		}
	}
	if len(runtime.seen) != len(scopedRoutes) || probe.calls != len(scopedRoutes) || !probe.bound {
		t.Fatalf("calls=%d scopes=%d", probe.calls, len(runtime.seen))
	}
	for _, scope := range runtime.seen {
		if scope.TenantID != authTenant || scope.OrgID != authOrg || scope.UserID != authUUID || scope.Token() == "" {
			t.Fatalf("wrong bound scope: %+v", scope)
		}
	}
}

func TestS1JWTRequiresEveryTimeClaimAndExactServingAudience(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(0, 0)
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"iat", "nbf"} {
		claims := claimsFixture(at)
		delete(claims, field)
		if _, verifyErr := verifier.Verify(context.Background(), signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims)); verifyErr == nil {
			t.Errorf("missing %s accepted", field)
		}
	}
	claims := claimsFixture(at)
	claims["aud"] = []string{"umir-serving"}
	if _, verifyErr := verifier.Verify(context.Background(), signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims)); verifyErr == nil {
		t.Error("incorrect operation audiences accepted")
	}
}

func TestS1MissingWarehouseContextCannotInventSuccessfulMetadata(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &authorizedRuntimeStub{at: at, omitObservation: true}
	handler, err := NewScopedHandler(&scopeResolverProbe{runtime: runtime}, verifier, ScopedOptions{Options: Options{RequestTimeout: time.Second, MaxRequestBytes: 1024}, GatewayKey: "gateway-test", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	claims := claimsFixture(at)
	claims["operation"] = "confirmOrder"
	claims["permissions"] = []string{"order.confirm", "consumer.order.evaluate", "consumer.order.commit", "consumer.order.resolve"}
	request := httptest.NewRequest("POST", "/internal/serving/v2/r/"+authTenant+"/order-confirmations", strings.NewReader(`{"commandID":"c","proposalID":"p"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Umir-Service-Key", "gateway-test")
	request.Header.Set("X-Umir-Runtime-Epoch", authEpoch)
	request.Header.Set("Authorization", "Bearer "+signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"outcome":"unknown"`) {
		t.Fatalf("fabricated response: %d %s", response.Code, response.Body.String())
	}
}

func TestS1ServingCatalogRequiresFrozenMenuReadCapability(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.FixedZone("MSK", 10800))
	verifier, err := NewJWTVerifier(JWTOptions{Now: func() time.Time { return at }, Keys: func(context.Context, string) (ed25519.PublicKey, error) { return pub, nil }})
	if err != nil {
		t.Fatal(err)
	}
	probe := &scopeResolverProbe{runtime: &authorizedRuntimeStub{at: at}}
	handler, err := NewScopedHandler(probe, verifier, ScopedOptions{Options: Options{RequestTimeout: time.Second, MaxRequestBytes: 1024}, GatewayKey: "gateway-test", Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []struct{ path, operation string }{{"products", "servingProducts"}, {"dish-catalog", "servingDishes"}, {"dish?dishID=soup", "servingDish"}} {
		claims := claimsFixture(at)
		claims["operation"] = route.operation
		claims["permissions"] = []string{"catalog.read"}
		request := httptest.NewRequest("GET", "/internal/serving/v2/r/"+authTenant+"/"+route.path, nil)
		request.Header.Set("X-Umir-Service-Key", "gateway-test")
		request.Header.Set("Authorization", "Bearer "+signFixture(t, key, map[string]any{"alg": "EdDSA", "typ": "JWT", "kid": "test"}, claims))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 403 || probe.calls != 0 {
			t.Fatalf("consumer grant authorized %s: %d calls=%d %s", route.path, response.Code, probe.calls, response.Body.String())
		}
	}
}
