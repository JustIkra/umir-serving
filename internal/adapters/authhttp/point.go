package authhttp

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/httpjson"
)

type RestaurantPoint struct {
	Revision  string
	ETag      string
	State     string
	Entry     *Entry
	Tombstone *Tombstone
}

type restaurantPointWire struct {
	Revision  string     `json:"registryRevision"`
	State     string     `json:"state"`
	Entry     *Entry     `json:"entry,omitempty"`
	Tombstone *Tombstone `json:"tombstone,omitempty"`
}

func (c *Client) RestaurantPoint(ctx context.Context, restaurantID, etag string) (point RestaurantPoint, notModified bool, result error) {
	if !contract.ValidEntityID(restaurantID) || (etag != "" && !validETag(etag)) {
		return RestaurantPoint{}, false, unavailable(contract.ErrInvalidArgument)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	path := "/internal/auth/v1/registry/" + url.PathEscape(restaurantID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return RestaurantPoint{}, false, unavailable(err)
	}
	request.Header.Set("X-Umir-Service-Key", c.key)
	request.Header.Set("Accept", "application/json")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return RestaurantPoint{}, false, unavailable(err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil {
			result = errors.Join(result, unavailable(closeErr))
		}
	}()
	if response.StatusCode == http.StatusNotModified && etag != "" {
		if response.Header.Get("ETag") != etag {
			return RestaurantPoint{}, false, unavailable(errors.New("auth point response has an invalid ETag"))
		}
		return RestaurantPoint{}, true, nil
	}
	if response.StatusCode == http.StatusNotFound {
		return RestaurantPoint{}, false, &contract.OperationError{Code: "RuntimeNotReady", Message: "restaurant is absent from Auth registry", Outcome: "unavailable", Retryable: true}
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || contentType != "application/json" {
		return RestaurantPoint{}, false, unavailable(errors.New("invalid Auth point response status or media type"))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return RestaurantPoint{}, false, unavailable(errors.New("auth point response exceeds its bounded budget"))
	}
	var wire restaurantPointWire
	if err = httpjson.DecodeStrict(data, &wire); err != nil {
		return RestaurantPoint{}, false, unavailable(err)
	}
	if !contract.ValidVersion(wire.Revision) {
		return RestaurantPoint{}, false, unavailable(errors.New("invalid Auth registry revision"))
	}
	responseETag := response.Header.Get("ETag")
	if !validETag(responseETag) {
		return RestaurantPoint{}, false, unavailable(errors.New("invalid Auth point ETag"))
	}
	etagRevision, err := strconv.Unquote(responseETag)
	if err != nil || etagRevision != wire.Revision {
		return RestaurantPoint{}, false, unavailable(errors.New("auth point ETag does not match its revision"))
	}
	point = RestaurantPoint{Revision: wire.Revision, ETag: responseETag, State: wire.State, Entry: wire.Entry, Tombstone: wire.Tombstone}
	switch wire.State {
	case "live":
		if wire.Entry == nil || wire.Tombstone != nil || validateRegistryEntry(restaurantID, *wire.Entry) != nil {
			return RestaurantPoint{}, false, unavailable(errors.New("invalid Auth live registry point"))
		}
	case "tombstone":
		if wire.Entry != nil || wire.Tombstone == nil || wire.Tombstone.ID != restaurantID || !contract.ValidEntityID(wire.Tombstone.OrgID) || !contract.ValidVersion(wire.Tombstone.Version) {
			return RestaurantPoint{}, false, unavailable(errors.New("invalid Auth registry tombstone"))
		}
	default:
		return RestaurantPoint{}, false, unavailable(errors.New("unknown Auth registry point state"))
	}
	return point, false, nil
}

func validETag(value string) bool {
	if len(value) < 3 || value[0] != '"' || value[len(value)-1] != '"' {
		return false
	}
	revision, err := strconv.Unquote(value)
	return err == nil && contract.ValidVersion(revision)
}

func validateRegistryEntry(restaurantID string, entry Entry) error {
	restaurant := entry.Restaurant
	if restaurant.ID != restaurantID || !contract.ValidEntityID(restaurant.OrgID) || !contract.ValidVersion(restaurant.Version) || restaurant.Name == "" || len([]rune(restaurant.Name)) > 100 || !contract.ValidVersion(entry.Settings.Version) || entry.Settings.TimeZone == "" || len(entry.Settings.TimeZone) > 128 || entry.Settings.TimeZone == "Local" || entry.Services == nil || len(entry.Services) > 2 {
		return errors.New("invalid Auth registry entry")
	}
	if _, err := time.LoadLocation(entry.Settings.TimeZone); err != nil {
		return err
	}
	switch restaurant.Lifecycle {
	case "registering", "ready", "deleting", "deleted":
	default:
		return errors.New("invalid Auth lifecycle")
	}
	seen := make(map[string]bool, len(entry.Services))
	for _, peer := range entry.Services {
		if peer.RestaurantID != restaurant.ID || peer.Version != restaurant.Version || !contract.ValidEntityID(peer.Epoch) || seen[peer.Service] || (peer.Service != "warehouse" && peer.Service != "serving") || (peer.State != "ready" && peer.State != "pending" && peer.State != "closed") {
			return errors.New("invalid Auth peer readiness")
		}
		seen[peer.Service] = true
	}
	return nil
}
