package authhttp

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpjson"
)

type Client struct {
	baseURL     string
	key         string
	http        *http.Client
	timeout     time.Duration
	now         func() time.Time
	retryNow    func() time.Time
	keyGate     chan struct{}
	keys        map[string]ed25519.PublicKey
	loadedAt    time.Time
	attemptedAt time.Time
	unknown     map[string]bool
}

func New(baseURL, key string, client *http.Client, timeout time.Duration, now func() time.Time) (*Client, error) {
	return NewWithRetryClock(baseURL, key, client, timeout, now, time.Now)
}

func NewWithRetryClock(baseURL, key string, client *http.Client, timeout time.Duration, now, retryNow func() time.Time) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || key == "" || client == nil || timeout <= 0 || timeout > 5*time.Second || now == nil || retryNow == nil {
		return nil, contract.ErrInvalidArgument
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{baseURL: baseURL, key: key, http: &copyClient, timeout: timeout, now: now, retryNow: retryNow, unknown: make(map[string]bool), keyGate: make(chan struct{}, 1)}, nil
}
func unavailable(cause error) error {
	return &contract.OperationError{Code: "AuthUnavailable", Message: "Auth discovery is unavailable", Outcome: "unavailable", Retryable: true, Cause: cause}
}
func (c *Client) get(ctx context.Context, path string, output any) (result error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return unavailable(err)
	}
	r.Header.Set("X-Umir-Service-Key", c.key)
	r.Header.Set("Accept", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		return unavailable(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			result = errors.Join(result, unavailable(err))
		}
	}()
	content, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || content != "application/json" {
		return unavailable(errors.New("invalid Auth response status or media type"))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return unavailable(errors.New("auth response exceeds its bounded budget"))
	}
	if err = httpjson.DecodeStrict(data, output); err != nil {
		return unavailable(err)
	}
	return nil
}

type publicKey struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	ID        string `json:"kid"`
	X         string `json:"x"`
}

func (c *Client) lockKeys(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return unavailable(err)
	}
	select {
	case c.keyGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-c.keyGate
			return unavailable(err)
		}
		return nil
	case <-ctx.Done():
		return unavailable(ctx.Err())
	}
}

func (c *Client) SigningKey(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	if kid == "" || len(kid) > 256 {
		return nil, unavailable(errors.New("invalid signing key identity"))
	}
	if err := c.lockKeys(ctx); err != nil {
		return nil, err
	}
	defer func() { <-c.keyGate }()
	now := c.now()
	if now.IsZero() {
		return nil, unavailable(errors.New("invalid discovery clock"))
	}
	fresh := !c.loadedAt.IsZero() && !now.Before(c.loadedAt) && now.Sub(c.loadedAt) < 30*time.Second
	if fresh {
		if key, ok := c.keys[kid]; ok {
			return append(ed25519.PublicKey(nil), key...), nil
		}
		if c.unknown[kid] || len(c.unknown) >= 256 {
			return nil, unavailable(errors.New("unknown signing key"))
		}
		c.unknown[kid] = true
	}
	if !fresh && c.retryPending() {
		return nil, unavailable(errors.New("signing key refresh is pending"))
	}
	if err := c.refreshKeys(ctx, now); err != nil {
		return nil, err
	}
	if !fresh {
		c.unknown = make(map[string]bool)
	}
	key, ok := c.keys[kid]
	if !ok {
		c.unknown[kid] = true
		return nil, unavailable(errors.New("unknown signing key"))
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

func (c *Client) refreshKeys(ctx context.Context, now time.Time) error {
	retryNow := c.retryNow()
	if retryNow.IsZero() {
		return unavailable(errors.New("invalid transport retry clock"))
	}
	c.attemptedAt = retryNow
	var jwks struct {
		Keys []publicKey `json:"keys"`
	}
	if err := c.get(ctx, "/internal/auth/v1/jwks", &jwks); err != nil {
		return err
	}
	if len(jwks.Keys) < 1 || len(jwks.Keys) > 3 {
		return unavailable(errors.New("invalid signing key set"))
	}
	keys := make(map[string]ed25519.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		bytes, err := base64.RawURLEncoding.Strict().DecodeString(k.X)
		if err != nil || len(bytes) != ed25519.PublicKeySize || k.KeyType != "OKP" || k.Curve != "Ed25519" || k.Algorithm != "EdDSA" || k.Use != "sig" || strings.TrimSpace(k.ID) != k.ID || k.ID == "" || len(k.ID) > 256 || keys[k.ID] != nil {
			return unavailable(errors.New("invalid Auth public signing key"))
		}
		keys[k.ID] = ed25519.PublicKey(bytes)
	}
	c.keys = keys
	c.loadedAt = now
	return nil
}

func (c *Client) retryPending() bool {
	if c.attemptedAt.IsZero() {
		return false
	}
	now := c.retryNow()
	if now.IsZero() {
		return true
	}
	if now.Before(c.attemptedAt) {
		c.attemptedAt = now
	}
	return now.Sub(c.attemptedAt) < time.Second
}

func (c *Client) CheckSigningKeys(ctx context.Context) error {
	if err := c.lockKeys(ctx); err != nil {
		return err
	}
	defer func() { <-c.keyGate }()
	now := c.now()
	if now.IsZero() {
		return unavailable(errors.New("invalid discovery clock"))
	}
	if !c.loadedAt.IsZero() && !now.Before(c.loadedAt) && now.Sub(c.loadedAt) < 30*time.Second {
		return nil
	}
	if c.retryPending() {
		return unavailable(errors.New("signing key refresh is pending"))
	}
	if err := c.refreshKeys(ctx, now); err != nil {
		return err
	}
	c.unknown = make(map[string]bool)
	return nil
}

type Settings struct {
	TimeZone string `json:"timeZone"`
	Version  string `json:"settingsVersion"`
}
type Restaurant struct {
	ID        string `json:"restaurantID"`
	OrgID     string `json:"orgID"`
	Name      string `json:"name"`
	Lifecycle string `json:"lifecycle"`
	Version   string `json:"lifecycleVersion"`
}
type Readiness struct {
	RestaurantID string `json:"restaurantID"`
	Service      string `json:"service"`
	Epoch        string `json:"epoch"`
	State        string `json:"state"`
	Version      string `json:"lifecycleVersion"`
}
type Entry struct {
	Restaurant Restaurant  `json:"restaurant"`
	Settings   Settings    `json:"settings"`
	Services   []Readiness `json:"services"`
}
type Tombstone struct {
	ID      string `json:"restaurantID"`
	OrgID   string `json:"orgID"`
	Version string `json:"lifecycleVersion"`
}
type Snapshot struct {
	Revision   string      `json:"registryRevision"`
	Complete   *bool       `json:"complete"`
	Entries    []Entry     `json:"entries"`
	Tombstones []Tombstone `json:"tombstones"`
}

func (c *Client) Registry(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	if err := c.get(ctx, "/internal/auth/v1/registry", &s); err != nil {
		return Snapshot{}, err
	}
	if s.Complete == nil || !*s.Complete || !contract.ValidVersion(s.Revision) || s.Entries == nil || s.Tombstones == nil || len(s.Entries) > 10000 || len(s.Tombstones) > 10000 {
		return Snapshot{}, unavailable(errors.New("incomplete Auth registry"))
	}
	seen := make(map[string]Restaurant)
	for _, e := range s.Entries {
		r := e.Restaurant
		if !contract.ValidEntityID(r.ID) || !contract.ValidEntityID(r.OrgID) || !contract.ValidVersion(r.Version) || r.Name == "" || len([]rune(r.Name)) > 100 || seen[r.ID].ID != "" || !contract.ValidVersion(e.Settings.Version) || e.Settings.TimeZone == "" || len(e.Settings.TimeZone) > 128 || e.Settings.TimeZone == "Local" || e.Services == nil || len(e.Services) > 3 {
			return Snapshot{}, unavailable(errors.New("invalid registry entry"))
		}
		if _, err := time.LoadLocation(e.Settings.TimeZone); err != nil {
			return Snapshot{}, unavailable(err)
		}
		switch r.Lifecycle {
		case "registering", "ready", "deleting", "deleted":
		default:
			return Snapshot{}, unavailable(errors.New("invalid restaurant lifecycle"))
		}
		services := make(map[string]bool)
		for _, p := range e.Services {
			if p.RestaurantID != r.ID || p.Version != r.Version || !contract.ValidEntityID(p.Epoch) || services[p.Service] || (p.Service != "warehouse" && p.Service != "serving" && p.Service != "gateway") || (p.State != "ready" && p.State != "pending" && p.State != "closed") {
				return Snapshot{}, unavailable(errors.New("invalid peer readiness"))
			}
			services[p.Service] = true
		}
		seen[r.ID] = r
	}
	tombstones := make(map[string]bool)
	for _, r := range s.Tombstones {
		previous := seen[r.ID]
		if !contract.ValidEntityID(r.ID) || !contract.ValidEntityID(r.OrgID) || !contract.ValidVersion(r.Version) || tombstones[r.ID] || (previous.ID != "" && (previous.OrgID != r.OrgID || previous.Version != r.Version || (previous.Lifecycle != "deleting" && previous.Lifecycle != "deleted"))) {
			return Snapshot{}, unavailable(errors.New("invalid registry tombstone"))
		}
		tombstones[r.ID] = true
	}
	return s, nil
}
func (s Snapshot) Projection() contract.RegistrySnapshot {
	p := contract.RegistrySnapshot{Revision: s.Revision, Complete: s.Complete != nil && *s.Complete}
	for _, e := range s.Entries {
		r := e.Restaurant
		p.Entries = append(p.Entries, contract.RegistryRestaurant{OrgID: r.OrgID, TenantID: r.ID, LifecycleVersion: r.Version, Lifecycle: r.Lifecycle})
	}
	for _, r := range s.Tombstones {
		p.Tombstones = append(p.Tombstones, contract.RegistryRestaurant{OrgID: r.OrgID, TenantID: r.ID, LifecycleVersion: r.Version, Lifecycle: "deleted"})
	}
	return p
}
func (c *Client) WarehouseEpoch(ctx context.Context, tenant, servingEpoch string) (string, error) {
	snapshot, err := c.Registry(ctx)
	if err != nil {
		return "", err
	}
	scope, business := contract.RequestScopeFromContext(ctx)
	for _, t := range snapshot.Tombstones {
		if t.ID == tenant {
			return "", &contract.OperationError{Code: "ScopeDeleted", Outcome: "unavailable"}
		}
	}
	for _, e := range snapshot.Entries {
		if e.Restaurant.ID != tenant {
			continue
		}
		if business && (e.Restaurant.OrgID != scope.OrgID || e.Restaurant.Version != scope.LifecycleVersion) {
			return "", &contract.OperationError{Code: "Forbidden", Outcome: "unavailable"}
		}
		if e.Restaurant.Lifecycle == "deleting" || e.Restaurant.Lifecycle == "deleted" {
			return "", &contract.OperationError{Code: "ScopeDeleted", Outcome: "unavailable"}
		}
		warehouse := ""
		serving := ""
		for _, p := range e.Services {
			if p.State == "ready" {
				switch p.Service {
				case "warehouse":
					warehouse = p.Epoch
				case "serving":
					serving = p.Epoch
				}
			}
		}
		if warehouse == "" || (business && (serving != servingEpoch || e.Restaurant.Lifecycle != "ready")) {
			return "", &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable", Retryable: true}
		}
		return warehouse, nil
	}
	return "", &contract.OperationError{Code: "RuntimeNotReady", Outcome: "unavailable", Retryable: true}
}
