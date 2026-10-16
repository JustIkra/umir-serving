//go:build integration

package storecontract_test

import (
	"testing"
	"time"

	"github.com/JustIkra/umir-serving/internal/adapters/pgstore"
	"github.com/JustIkra/umir-serving/tests/pgtest"
	"github.com/JustIkra/umir-serving/tests/storecontract"
)

const operationID = "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3"

func TestPostgresStore(t *testing.T) {
	storecontract.Run(t, func(t *testing.T, options storecontract.Options) storecontract.Fixture {
		env := pgtest.New(t)
		for _, restaurant := range []string{restaurantA, restaurantB} {
			env.Admin(t, `INSERT INTO serving_restaurants (restaurant_id, org_id, lifecycle_version, time_zone, settings_version, operation_id, state, registered_at, updated_at)
				VALUES ($1, $2, 1, 'Europe/Moscow', 1, $3, 'registered', now(), now())`, restaurant, orgA, operationID)
		}
		clock := &fakeClock{now: time.Now().UTC()}
		backend := pgstore.New(env.OpenRuntime(t, 20), pgstore.Options{MaxActiveProposals: options.MaxActiveProposals, Clock: clock.Now})
		return storecontract.Fixture{
			Store: backend.Store(orgA, restaurantA), Other: backend.Store(orgA, restaurantB), Advance: clock.Advance, Now: clock.Now,
			Sweep: backend.Sweep, PickDue: backend.PickDue, OrgID: orgA, RestaurantID: restaurantA,
		}
	})
}
