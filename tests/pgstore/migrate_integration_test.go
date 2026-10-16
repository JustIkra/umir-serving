//go:build integration

package pgstore_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-serving/internal/adapters/pgstore"
	"github.com/JustIkra/umir-serving/internal/adapters/postgres"
	"github.com/JustIkra/umir-serving/tests/pgtest"
)

func TestMigrationsAreIdempotentAndParallel(t *testing.T) {
	env := pgtest.NewUnmigrated(t)
	owner := env.OpenOwner(t)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 { // two replicas starting the migrate job together
		wg.Go(func() { errs <- pgstore.Migrate(t.Context(), owner) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel migrate: %v", err)
		}
	}
	if err := pgstore.Migrate(t.Context(), owner); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	runtime := env.OpenRuntime(t, 2)
	if err := pgstore.VerifySchema(t.Context(), runtime); err != nil {
		t.Fatalf("verify schema as runtime: %v", err)
	}
	generation, err := postgres.Generation(t.Context(), runtime)
	if err != nil || len(generation) != 36 {
		t.Fatalf("generation = %q, %v", generation, err)
	}
	var count int
	err = runtime.InTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE tablename LIKE 'serving_%'`).Scan(&count)
	})
	if err != nil || count != 3 {
		t.Fatalf("policies = %d, %v", count, err)
	}
}

func TestRLSPoliciesAreTheReferenceOnes(t *testing.T) {
	for _, table := range []string{"serving_restaurants", "serving_proposals", "serving_confirmation_attempts"} {
		want, err := postgres.RLSPolicySQL(table, "restaurant_id", postgres.RLSUUIDColumn())
		if err != nil {
			t.Fatal(err)
		}
		sql, err := migrationText()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(sql, want) {
			t.Fatalf("migration lacks the reference policy of %s", table)
		}
	}
}

func TestRuntimeRoleIsNotAnOwner(t *testing.T) {
	env := pgtest.New(t)
	if err := postgres.AssertRuntimeRole(t.Context(), env.OpenRuntime(t, 2)); err != nil {
		t.Fatalf("runtime role: %v", err)
	}
	if err := postgres.AssertRuntimeRole(t.Context(), env.OpenOwner(t)); err == nil {
		t.Fatal("the owner role passed AssertRuntimeRole")
	}
}

func migrationText() (string, error) {
	data, err := os.ReadFile("../../internal/adapters/pgstore/migrations/0001_init.sql")
	return string(data), err
}
