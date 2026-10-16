//go:build integration

package pgstore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-serving/internal/adapters/pgstore"
	"github.com/JustIkra/umir-serving/internal/adapters/postgres"
	"github.com/JustIkra/umir-serving/internal/domain"
	"github.com/JustIkra/umir-serving/tests/pgtest"
)

const (
	orgA   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	orgB   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	restA  = "a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1"
	restB  = "b2b2b2b2-b2b2-4b2b-8b2b-b2b2b2b2b2b2"
	opID   = "c3c3c3c3-c3c3-4c3c-8c3c-c3c3c3c3c3c3"
	insert = `INSERT INTO serving_restaurants (restaurant_id, org_id, lifecycle_version, time_zone, settings_version, operation_id, state, registered_at, updated_at)
		VALUES ($1, $2, 1, 'Europe/Moscow', 1, $3, 'registered', now(), now())`
)

func countRows(t *testing.T, db *postgres.DB, scope *postgres.Scope, system string, table string) int {
	t.Helper()
	var n int
	count := func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	}
	var err error
	switch {
	case system != "":
		err = db.InSystemTx(t.Context(), system, pgx.TxOptions{}, count)
	case scope != nil:
		err = db.InScopedTx(t.Context(), *scope, pgx.TxOptions{}, count)
	default:
		err = db.InTx(t.Context(), pgx.TxOptions{}, count)
	}
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestRowLevelSecurityOnTheRealRuntimeRole(t *testing.T) {
	env := pgtest.New(t)
	env.Admin(t, insert, restA, orgA, opID)
	env.Admin(t, insert, restB, orgB, opID)
	// One connection: whatever a transaction leaves behind would show up in the next.
	db := env.OpenRuntime(t, 1)
	backend := pgstore.New(db, pgstore.Options{})
	for i, store := range []*pgstore.Store{backend.Store(orgA, restA), backend.Store(orgB, restB)} {
		for j := range 3 {
			p := domain.OrderProposal{ProposalID: fmt.Sprintf("p-%d-%d", i, j), EvaluatedAt: time.Now(), AcceptedPositions: []domain.AcceptedPosition{{LineNo: 1, DishID: "d", Portions: 1}}}
			if err := store.Create(t.Context(), p); err != nil {
				t.Fatal(err)
			}
		}
	}
	scopeA := postgres.Scope{Actor: postgres.ActorTenant, OrgID: orgA, RestaurantID: restA}
	for _, table := range []string{"serving_restaurants", "serving_proposals", "serving_confirmation_attempts"} {
		if n := countRows(t, db, nil, "", table); n != 0 {
			t.Fatalf("%s: a transaction without context sees %d rows", table, n)
		}
	}
	if n := countRows(t, db, &scopeA, "", "serving_proposals"); n != 3 {
		t.Fatalf("scope A sees %d proposals, want its 3", n)
	}
	if n := countRows(t, db, nil, "", "serving_proposals"); n != 0 {
		t.Fatalf("the scope of restaurant A leaked through the pool: %d rows", n)
	}
	if n := countRows(t, db, nil, "reconciler-pick", "serving_proposals"); n != 6 {
		t.Fatalf("a system transaction sees %d proposals, want 6", n)
	}
	if n := countRows(t, db, nil, "", "serving_proposals"); n != 0 {
		t.Fatalf("the system actor leaked through the pool: %d rows", n)
	}
	// A transaction of restaurant A cannot plant or read a row of restaurant B.
	err := db.InScopedTx(t.Context(), scopeA, pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO serving_proposals (restaurant_id, org_id, proposal_id, context, evaluated_at, requested_positions, accepted_positions, changes, position_count, created_at)
			VALUES ($1, $2, 'planted', '{}', now(), '[]', '[]', '[]', 0, now())`, restB, orgB)
		return err
	})
	if err == nil {
		t.Fatal("restaurant A planted a row of restaurant B")
	}
	if _, found, err := backend.Store(orgA, restA).Get(t.Context(), "p-1-0"); err != nil || found {
		t.Fatalf("restaurant A read a proposal of restaurant B: %v %v", found, err)
	}
}

func TestRuntimeRoleCannotChangeTheSchemaOrTheGeneration(t *testing.T) {
	env := pgtest.New(t)
	db := env.OpenRuntime(t, 2)
	for _, stmt := range []string{
		"CREATE TABLE intruder (id int)",
		"UPDATE storage_meta SET generation = gen_random_uuid()",
		"DELETE FROM schema_migrations",
		"ALTER TABLE serving_proposals DISABLE ROW LEVEL SECURITY",
	} {
		err := db.InTx(t.Context(), pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt)
			return err
		})
		if err == nil {
			t.Fatalf("the runtime role ran %q", stmt)
		}
	}
}

func TestHotPathsUseIndexesOnLargeTables(t *testing.T) {
	env := pgtest.New(t)
	const restaurants, perRestaurant = 50, 2000 // 100 000 proposals and as many attempts
	env.Admin(t, `INSERT INTO serving_restaurants (restaurant_id, org_id, lifecycle_version, time_zone, settings_version, operation_id, state, registered_at, updated_at)
		SELECT ('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, $1, 1, 'Europe/Moscow', 1, $2, 'registered', now(), now()
		FROM generate_series(1, $3::int) g`, orgA, opID, restaurants)
	env.Admin(t, `INSERT INTO serving_proposals (restaurant_id, org_id, proposal_id, context, evaluated_at, requested_positions, accepted_positions, changes, position_count, created_at)
		SELECT r.restaurant_id, r.org_id, 'p-' || n, '{}', now() - (CASE WHEN n % 500 = 0 THEN interval '30 hours' ELSE interval '1 minute' END), '[]', '[]', '[]', 0, now()
		FROM serving_restaurants r, generate_series(1, $1::int) n`, perRestaurant)
	env.Admin(t, `INSERT INTO serving_confirmation_attempts (restaurant_id, org_id, command_id, proposal_id, fingerprint, state, pending, context, accepted_positions, result, next_check_at, created_at, updated_at)
		SELECT r.restaurant_id, r.org_id, 'c-' || n, 'p-' || n, 'f', CASE WHEN n % 10 = 0 THEN 'Pending' ELSE 'Completed' END, n % 10 = 0 AND n % 20 = 0, '{}', '[]',
		       CASE WHEN n % 10 = 0 THEN NULL ELSE '{}'::jsonb END,
		       CASE WHEN n % 20 = 0 THEN now() - interval '1 minute' END,
		       now() - (CASE WHEN n % 700 = 0 THEN interval '40 days' ELSE interval '1 hour' END), now() - (CASE WHEN n % 700 = 0 THEN interval '40 days' ELSE interval '1 hour' END)
		FROM serving_restaurants r, generate_series(1, $1::int) n`, perRestaurant)
	env.Admin(t, "ANALYZE")
	db := env.OpenRuntime(t, 2)
	rest := "00000000-0000-4000-8000-000000000007"
	queries := map[string]string{
		"proposal by key":     fmt.Sprintf(`SELECT context FROM serving_proposals WHERE restaurant_id = '%s' AND proposal_id = 'p-77'`, rest),
		"pending owner":       fmt.Sprintf(`SELECT command_id FROM serving_confirmation_attempts WHERE restaurant_id = '%s' AND proposal_id = 'p-80' AND pending`, rest),
		"attempt by key":      fmt.Sprintf(`SELECT state FROM serving_confirmation_attempts WHERE restaurant_id = '%s' AND command_id = 'c-77'`, rest),
		"active proposals":    fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM serving_proposals WHERE restaurant_id = '%s' AND settled_at IS NULL AND evaluated_at > now() - interval '24 hours' LIMIT 2000) a`, rest),
		"lease":               fmt.Sprintf(`UPDATE serving_proposals SET lease_seq = lease_seq + 1 WHERE restaurant_id = '%s' AND proposal_id = 'p-77' AND (lease_until IS NULL OR lease_until <= now())`, rest),
		"restaurant row":      fmt.Sprintf(`SELECT state FROM serving_restaurants WHERE restaurant_id = '%s' AND org_id = '%s' FOR SHARE`, rest, orgA),
		"due attempts":        `SELECT a.command_id FROM serving_confirmation_attempts a JOIN serving_restaurants r ON r.restaurant_id = a.restaurant_id WHERE a.pending AND a.unresolved_at IS NULL AND r.state = 'registered' AND a.next_check_at <= now() ORDER BY a.next_check_at LIMIT 50 FOR UPDATE OF a SKIP LOCKED`,
		"sweep old terminals": `DELETE FROM serving_confirmation_attempts WHERE ctid IN (SELECT ctid FROM serving_confirmation_attempts WHERE NOT pending AND state IN ('Completed', 'Rejected') AND updated_at < now() - interval '30 days' LIMIT 1000 FOR UPDATE SKIP LOCKED)`,
		"sweep unsent":        `DELETE FROM serving_confirmation_attempts WHERE ctid IN (SELECT ctid FROM serving_confirmation_attempts WHERE state = 'Pending' AND NOT pending AND warehouse_result IS NULL AND updated_at < now() - interval '24 hours' LIMIT 1000 FOR UPDATE SKIP LOCKED)`,
		"sweep old proposals": `DELETE FROM serving_proposals WHERE ctid IN (SELECT p.ctid FROM serving_proposals p WHERE p.evaluated_at < now() - interval '24 hours' AND NOT EXISTS (SELECT 1 FROM serving_confirmation_attempts a WHERE a.restaurant_id = p.restaurant_id AND a.proposal_id = p.proposal_id) LIMIT 1000 FOR UPDATE OF p SKIP LOCKED)`,
	}
	for name, query := range queries {
		var plan string
		err := db.InSystemTx(t.Context(), "plan-check", pgx.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, "EXPLAIN "+query)
			if err != nil {
				return err
			}
			defer rows.Close()
			var lines []string
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				lines = append(lines, line)
			}
			plan = strings.Join(lines, "\n")
			return rows.Err()
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(plan, "Seq Scan on serving_proposals") || strings.Contains(plan, "Seq Scan on serving_confirmation_attempts") {
			t.Errorf("%s uses a sequential scan:\n%s", name, plan)
		}
	}
}
