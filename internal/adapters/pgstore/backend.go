package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/adapters/postgres"
	"github.com/JustIkra/umir-serving/internal/application"
)

const (
	stateRegistered = "registered"
	stateClosed     = "closed"
)

// Options tune the backend. Zero values are the production defaults.
type Options struct {
	// MaxActiveProposals caps unsettled proposals younger than 24 hours per
	// restaurant (G1-5); zero means no cap.
	MaxActiveProposals int
	// Clock replaces the database clock in tests; nil uses the database clock,
	// the one time source of all replicas.
	Clock func() time.Time
}

// Backend is the PostgreSQL storage of one Serving cell: the per-restaurant
// Store, the registrations and the passes that cross restaurants.
type Backend struct {
	db   *postgres.DB
	opts Options
}

// New wraps an opened pool (runtime role).
func New(db *postgres.DB, opts Options) *Backend { return &Backend{db: db, opts: opts} }

// Store returns the storage of one restaurant.
func (b *Backend) Store(orgID, restaurantID string) *Store {
	return &Store{backend: b, orgID: orgID, restaurantID: restaurantID}
}

// nowArg is the "current time" argument of statements: the injected clock, or
// nil so that the statement falls back to the database clock.
func (b *Backend) nowArg() any {
	if b.opts.Clock == nil {
		return nil
	}
	return b.opts.Clock().UTC()
}

func scopeOf(orgID, restaurantID string) postgres.Scope {
	return postgres.Scope{Actor: postgres.ActorTenant, OrgID: orgID, RestaurantID: restaurantID}
}

// systemTx runs fn as the system actor with a reason (R8, C5).
func (b *Backend) systemTx(ctx context.Context, reason string, fn func(context.Context, pgx.Tx) error) error {
	return mapError(b.db.InSystemTx(ctx, reason, pgx.TxOptions{}, fn))
}

var _ application.AttemptPicker = (*Backend)(nil)
var _ application.Sweeper = (*Backend)(nil)

// PickDue selects up to limit pending attempts of registered restaurants whose
// check is due and moves their next check forward by shift. Rows locked by
// another replica are skipped, so replicas do not pick the same work.
func (b *Backend) PickDue(ctx context.Context, limit int, shift time.Duration) ([]application.DueAttempt, error) {
	if limit <= 0 || shift <= 0 {
		return nil, contract.ErrInvalidArgument
	}
	var due []application.DueAttempt
	err := b.systemTx(ctx, "reconciler-pick", func(ctx context.Context, tx pgx.Tx) error {
		due = nil
		rows, err := tx.Query(ctx, `WITH due AS (
				SELECT a.restaurant_id, a.command_id
				  FROM serving_confirmation_attempts a
				  JOIN serving_restaurants r ON r.restaurant_id = a.restaurant_id
				 WHERE a.pending AND a.unresolved_at IS NULL AND r.state = 'registered'
				   AND a.next_check_at <= COALESCE($1::timestamptz, clock_timestamp())
				 ORDER BY a.next_check_at
				 LIMIT $2
				   FOR UPDATE OF a SKIP LOCKED)
			UPDATE serving_confirmation_attempts t
			   SET next_check_at = COALESCE($1::timestamptz, clock_timestamp()) + make_interval(secs => $3::float8)
			  FROM due
			 WHERE t.restaurant_id = due.restaurant_id AND t.command_id = due.command_id
			RETURNING t.org_id::text, t.restaurant_id::text, t.command_id`, b.nowArg(), limit, shift.Seconds())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d application.DueAttempt
			if err := rows.Scan(&d.OrgID, &d.RestaurantID, &d.CommandID); err != nil {
				return err
			}
			due = append(due, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return due, nil
}

// Sweep deletes rows past their retention (R9), at most limit rows per rule.
// It is safe to run on every replica: rows are taken with SKIP LOCKED.
func (b *Backend) Sweep(ctx context.Context, limit int) (application.SweepResult, error) {
	if limit <= 0 {
		return application.SweepResult{}, contract.ErrInvalidArgument
	}
	var result application.SweepResult
	err := b.systemTx(ctx, "ttl-sweep", func(ctx context.Context, tx pgx.Tx) error {
		result = application.SweepResult{}
		now := b.nowArg()
		run := func(sql string) (int, error) {
			tag, err := tx.Exec(ctx, sql, now, limit)
			return int(tag.RowsAffected()), err
		}
		var err error
		// Terminal attempts first: a proposal is deleted only once nothing refers to it.
		attemptsDone, err := run(`DELETE FROM serving_confirmation_attempts WHERE ctid IN (
			SELECT ctid FROM serving_confirmation_attempts
			 WHERE NOT pending AND state IN ('Completed', 'Rejected')
			   AND updated_at < COALESCE($1::timestamptz, clock_timestamp()) - interval '30 days'
			 LIMIT $2 FOR UPDATE SKIP LOCKED)`)
		if err != nil {
			return err
		}
		unsent, err := run(`DELETE FROM serving_confirmation_attempts WHERE ctid IN (
			SELECT ctid FROM serving_confirmation_attempts
			 WHERE state = 'Pending' AND NOT pending AND warehouse_result IS NULL
			   AND updated_at < COALESCE($1::timestamptz, clock_timestamp()) - interval '24 hours'
			 LIMIT $2 FOR UPDATE SKIP LOCKED)`)
		if err != nil {
			return err
		}
		proposals, err := run(`DELETE FROM serving_proposals WHERE ctid IN (
			SELECT p.ctid FROM serving_proposals p
			 WHERE p.evaluated_at < COALESCE($1::timestamptz, clock_timestamp()) - interval '24 hours'
			   AND NOT EXISTS (SELECT 1 FROM serving_confirmation_attempts a WHERE a.restaurant_id = p.restaurant_id AND a.proposal_id = p.proposal_id)
			 LIMIT $2 FOR UPDATE OF p SKIP LOCKED)`)
		if err != nil {
			return err
		}
		closedAttempts, err := run(`DELETE FROM serving_confirmation_attempts WHERE ctid IN (
			SELECT a.ctid FROM serving_confirmation_attempts a
			  JOIN serving_restaurants r ON r.restaurant_id = a.restaurant_id
			 WHERE r.state = 'closed' AND r.closed_at < COALESCE($1::timestamptz, clock_timestamp()) - interval '7 days'
			 LIMIT $2 FOR UPDATE OF a SKIP LOCKED)`)
		if err != nil {
			return err
		}
		closedProposals, err := run(`DELETE FROM serving_proposals WHERE ctid IN (
			SELECT p.ctid FROM serving_proposals p
			  JOIN serving_restaurants r ON r.restaurant_id = p.restaurant_id
			 WHERE r.state = 'closed' AND r.closed_at < COALESCE($1::timestamptz, clock_timestamp()) - interval '7 days'
			   AND NOT EXISTS (SELECT 1 FROM serving_confirmation_attempts a WHERE a.restaurant_id = p.restaurant_id AND a.proposal_id = p.proposal_id)
			 LIMIT $2 FOR UPDATE OF p SKIP LOCKED)`)
		if err != nil {
			return err
		}
		result = application.SweepResult{Proposals: proposals, Attempts: attemptsDone + unsent, ClosedRestaurant: closedAttempts + closedProposals}
		return nil
	})
	return result, err
}
