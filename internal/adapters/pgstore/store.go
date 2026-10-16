package pgstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/JustIkra/umir-serving/contract"
	"github.com/JustIkra/umir-serving/internal/application"
	"github.com/JustIkra/umir-serving/internal/domain"
)

const (
	activeProposalWindow = 24 * time.Hour

	sqlUniqueViolation  = "23505"
	sqlProgramLimit     = "54000"
	pendingIndexName    = "serving_attempts_one_pending"
	proposalsPrimaryKey = "serving_proposals_pkey"
)

// Store keeps the proposals and attempts of one restaurant. It implements
// application.ProposalStore and application.ConfirmationAttemptStore. Every
// operation is one short transaction; none is open while the Warehouse is called.
type Store struct {
	backend      *Backend
	orgID        string
	restaurantID string
}

var (
	_ application.ProposalStore            = (*Store)(nil)
	_ application.ConfirmationAttemptStore = (*Store)(nil)
)

// tx runs fn in a transaction scoped to the restaurant. A write first takes the
// restaurant row FOR SHARE: Close takes it FOR UPDATE, so once Close commits
// every later write fails with ScopeDeleted and the writes begun before it
// have finished.
func (s *Store) tx(ctx context.Context, write bool, fn func(context.Context, pgx.Tx) error) error {
	scope := scopeOf(s.orgID, s.restaurantID)
	opts := pgx.TxOptions{}
	if !write {
		opts.AccessMode = pgx.ReadOnly
	}
	err := s.backend.db.InScopedTx(ctx, scope, opts, func(ctx context.Context, tx pgx.Tx) error {
		if write {
			if err := s.fence(ctx, tx); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
	return mapError(err)
}

func (s *Store) fence(ctx context.Context, tx pgx.Tx) error {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM serving_restaurants WHERE restaurant_id = $1 AND org_id = $2 FOR SHARE`, s.restaurantID, s.orgID).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return &contract.OperationError{Code: "RuntimeNotReady", Outcome: "not_committed"}
	case err != nil:
		return err
	case state != stateRegistered:
		return &contract.OperationError{Code: "ScopeDeleted", Outcome: "not_committed"}
	}
	return nil
}

// mapError turns a driver error into the errors of the store contract. A
// context error stays as it is; everything else from the database is
// ErrStorageUnavailable, so callers can tell "the storage failed" apart.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var known *contract.OperationError
	switch {
	case errors.As(err, &known),
		errors.Is(err, domain.ErrInvalidProposal), errors.Is(err, domain.ErrProposalExists), errors.Is(err, domain.ErrProposalMissing),
		errors.Is(err, domain.ErrPendingMismatch), errors.Is(err, domain.ErrProposalReplaced), errors.Is(err, domain.ErrInvalidReplace),
		errors.Is(err, domain.ErrLeaseLost), errors.Is(err, contract.ErrConfirmationPending), errors.Is(err, contract.ErrCommandIdentityConflict),
		errors.Is(err, contract.ErrLimitExceeded), errors.Is(err, contract.ErrInvalidArgument):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlProgramLimit {
		// A value too long for an index row: a request error, not a failure.
		return contract.ErrInvalidArgument
	}
	return fmt.Errorf("%w: %w", contract.ErrStorageUnavailable, err)
}

func isUnique(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlUniqueViolation && pgErr.ConstraintName == constraint
}

func (s *Store) now() any { return s.backend.nowArg() }

func (s *Store) Create(ctx context.Context, p domain.OrderProposal) error {
	if p.ProposalID == "" || !contract.ValidClientID(p.ProposalID) {
		return domain.ErrInvalidProposal
	}
	doc, err := marshal(toProposalDoc(p))
	if err != nil {
		return err
	}
	requested, err := marshal(requestedDocs(p.RequestedPositions))
	if err != nil {
		return err
	}
	accepted, err := marshal(acceptedDocs(p.AcceptedPositions))
	if err != nil {
		return err
	}
	changes, err := marshal(changeDocs(p.Changes))
	if err != nil {
		return err
	}
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		if limit := s.backend.opts.MaxActiveProposals; limit > 0 {
			// The count and the insert are serialized per restaurant, so
			// parallel Prepare calls cannot overshoot the limit.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "serving.proposals/"+s.restaurantID); err != nil {
				return err
			}
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM (
				SELECT 1 FROM serving_proposals
				 WHERE restaurant_id = $1 AND settled_at IS NULL
				   AND evaluated_at > COALESCE($2::timestamptz, clock_timestamp()) - make_interval(secs => $3::float8)
				 LIMIT $4) active`, s.restaurantID, s.now(), activeProposalWindow.Seconds(), limit).Scan(&active); err != nil {
				return err
			}
			if active >= limit {
				return contract.ErrLimitExceeded
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO serving_proposals
			(restaurant_id, org_id, proposal_id, context, evaluated_at, requested_positions, accepted_positions, changes, position_count, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10::timestamptz, clock_timestamp()))`,
			s.restaurantID, s.orgID, p.ProposalID, doc, p.EvaluatedAt.UTC(), requested, accepted, changes, len(p.RequestedPositions), s.now())
		if isUnique(err, proposalsPrimaryKey) {
			return fmt.Errorf("%w: %s", domain.ErrProposalExists, p.ProposalID)
		}
		return err
	})
}

type proposalRow struct {
	context    []byte
	requested  []byte
	accepted   []byte
	changes    []byte
	supersedes *string
	pending    *string
}

func (s *Store) Get(ctx context.Context, proposalID string) (domain.ProposalRecord, bool, error) {
	var record domain.ProposalRecord
	found := false
	err := s.tx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		var row proposalRow
		err := tx.QueryRow(ctx, `SELECT p.context, p.requested_positions, p.accepted_positions, p.changes, p.superseded_by,
			(SELECT a.command_id FROM serving_confirmation_attempts a
			  WHERE a.restaurant_id = p.restaurant_id AND a.proposal_id = p.proposal_id AND a.pending)
			FROM serving_proposals p WHERE p.restaurant_id = $1 AND p.proposal_id = $2`, s.restaurantID, proposalID).
			Scan(&row.context, &row.requested, &row.accepted, &row.changes, &row.supersedes, &row.pending)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		record, err = row.record(proposalID)
		return err
	})
	if err != nil {
		return domain.ProposalRecord{}, false, err
	}
	return record, found, nil
}

func (r proposalRow) record(proposalID string) (domain.ProposalRecord, error) {
	var doc proposalDoc
	if err := unmarshal(r.context, &doc); err != nil {
		return domain.ProposalRecord{}, err
	}
	wc, err := doc.Context.warehouseContext()
	if err != nil {
		return domain.ProposalRecord{}, err
	}
	evaluatedAt, err := parseInstant(doc.EvaluatedAt)
	if err != nil {
		return domain.ProposalRecord{}, err
	}
	version, err := strconv.ParseInt(doc.StateVersion, 10, 64)
	if err != nil {
		return domain.ProposalRecord{}, fmt.Errorf("stored state version is invalid: %w", err)
	}
	var requested, accepted []positionDoc
	var changes []changeDoc
	for _, part := range []struct {
		data   []byte
		target any
	}{{r.requested, &requested}, {r.accepted, &accepted}, {r.changes, &changes}} {
		if err := unmarshal(part.data, part.target); err != nil {
			return domain.ProposalRecord{}, err
		}
	}
	proposal := domain.OrderProposal{
		Context:      domain.ProposalContext{OrgID: wc.OrgID, TenantID: wc.TenantID, Epoch: wc.Epoch, TimeZone: wc.TimeZone, SettingsVersion: wc.SettingsVersion, EvaluatedAt: wc.EvaluatedAt, StateVersion: wc.StateVersion},
		ProposalID:   proposalID,
		EvaluatedAt:  evaluatedAt,
		StateVersion: version,
	}
	if proposal.RequestedPositions, err = requestedFromDocs(requested); err != nil {
		return domain.ProposalRecord{}, err
	}
	if proposal.AcceptedPositions, err = acceptedFromDocs(accepted); err != nil {
		return domain.ProposalRecord{}, err
	}
	if proposal.Changes, err = changesFromDocs(changes); err != nil {
		return domain.ProposalRecord{}, err
	}
	record := domain.ProposalRecord{Proposal: proposal}
	if r.supersedes != nil {
		record.SupersededBy = *r.supersedes
	}
	if r.pending != nil {
		record.PendingCommandID = *r.pending
	}
	return record, nil
}

const attemptColumns = `proposal_id, fingerprint, state, pending, context, accepted_positions, warehouse_result, replacement, result, error,
	check_count, next_check_at, stuck_at, unresolved_at, created_at, updated_at`

type attemptRow struct {
	commandID, proposalID, fingerprint, state                string
	pending                                                  bool
	context, accepted                                        []byte
	warehouseResult, replacement, result, errDoc             []byte
	checkCount                                               int
	nextCheckAt, stuckAt, unresolvedAt, createdAt, updatedAt *time.Time
}

func (r *attemptRow) scan(row pgx.Row) error {
	return row.Scan(&r.proposalID, &r.fingerprint, &r.state, &r.pending, &r.context, &r.accepted, &r.warehouseResult, &r.replacement, &r.result, &r.errDoc,
		&r.checkCount, &r.nextCheckAt, &r.stuckAt, &r.unresolvedAt, &r.createdAt, &r.updatedAt)
}

func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func (r attemptRow) attempt() (contract.ConfirmationAttempt, error) {
	var ctxDoc contextDoc
	if err := unmarshal(r.context, &ctxDoc); err != nil {
		return contract.ConfirmationAttempt{}, err
	}
	wc, err := ctxDoc.warehouseContext()
	if err != nil {
		return contract.ConfirmationAttempt{}, err
	}
	var positions []positionDoc
	if err := unmarshal(r.accepted, &positions); err != nil {
		return contract.ConfirmationAttempt{}, err
	}
	accepted, err := contractAcceptedFromDocs(positions)
	if err != nil {
		return contract.ConfirmationAttempt{}, err
	}
	a := contract.ConfirmationAttempt{
		Context: wc, CommandID: r.commandID, ProposalID: r.proposalID, Fingerprint: r.fingerprint, AcceptedPositions: accepted,
		State: contract.OrderAttemptState(r.state), Pending: r.pending, CheckCount: r.checkCount,
		NextCheckAt: utc(r.nextCheckAt), StuckAt: utc(r.stuckAt), UnresolvedAt: utc(r.unresolvedAt), CreatedAt: utc(r.createdAt), UpdatedAt: utc(r.updatedAt),
	}
	if r.warehouseResult != nil {
		var value contract.CommitResult
		if err := unmarshal(r.warehouseResult, &value); err != nil {
			return contract.ConfirmationAttempt{}, err
		}
		a.WarehouseResult = &value
	}
	if r.replacement != nil {
		var value contract.OrderProposal
		if err := unmarshal(r.replacement, &value); err != nil {
			return contract.ConfirmationAttempt{}, err
		}
		a.Replacement = &value
	}
	if r.result != nil {
		var value contract.ConfirmResult
		if err := unmarshal(r.result, &value); err != nil {
			return contract.ConfirmationAttempt{}, err
		}
		a.Result = &value
	}
	if r.errDoc != nil {
		var value contract.AttemptError
		if err := unmarshal(r.errDoc, &value); err != nil {
			return contract.ConfirmationAttempt{}, err
		}
		a.Error = &value
	}
	return a, nil
}

func (s *Store) readAttempt(ctx context.Context, tx pgx.Tx, commandID string, lock string) (attemptRow, bool, error) {
	row := attemptRow{commandID: commandID}
	err := row.scan(tx.QueryRow(ctx, `SELECT `+attemptColumns+` FROM serving_confirmation_attempts WHERE restaurant_id = $1 AND command_id = $2`+lock, s.restaurantID, commandID))
	if errors.Is(err, pgx.ErrNoRows) {
		return attemptRow{}, false, nil
	}
	return row, err == nil, err
}

func (s *Store) GetAttempt(ctx context.Context, commandID string) (contract.ConfirmationAttempt, bool, error) {
	var attempt contract.ConfirmationAttempt
	found := false
	err := s.tx(ctx, false, func(ctx context.Context, tx pgx.Tx) error {
		row, ok, err := s.readAttempt(ctx, tx, commandID, "")
		if err != nil || !ok {
			return err
		}
		found = true
		attempt, err = row.attempt()
		return err
	})
	if err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	return attempt, found, nil
}

func (s *Store) BeginAttempt(ctx context.Context, a contract.ConfirmationAttempt) (contract.ConfirmationAttempt, bool, error) {
	if !contract.ValidClientID(a.CommandID) || !contract.ValidClientID(a.ProposalID) || a.Fingerprint == "" {
		return contract.ConfirmationAttempt{}, false, domain.ErrInvalidProposal
	}
	ctxDoc, err := marshal(warehouseContextDoc(a.Context))
	if err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	positions, err := marshal(contractAcceptedDocs(a.AcceptedPositions))
	if err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	var result contract.ConfirmationAttempt
	var conflict *contract.ConfirmationAttempt
	created := false
	err = s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		created, conflict = false, nil
		if existing, ok, err := s.readAttempt(ctx, tx, a.CommandID, ""); err != nil {
			return err
		} else if ok {
			return s.compare(existing, a, &result, &conflict)
		}
		var superseded *string
		proposalErr := tx.QueryRow(ctx, `SELECT superseded_by FROM serving_proposals WHERE restaurant_id = $1 AND proposal_id = $2 FOR SHARE`, s.restaurantID, a.ProposalID).Scan(&superseded)
		missing := errors.Is(proposalErr, pgx.ErrNoRows)
		if proposalErr != nil && !missing {
			return proposalErr
		}
		if missing && (a.Pending || len(a.AcceptedPositions) > 0) {
			return domain.ErrProposalMissing
		}
		if !missing && superseded != nil && a.Pending {
			return domain.ErrProposalReplaced
		}
		tag, err := tx.Exec(ctx, `INSERT INTO serving_confirmation_attempts
			(restaurant_id, org_id, command_id, proposal_id, fingerprint, state, pending, context, accepted_positions, next_check_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'Pending', $6, $7, $8,
			        CASE WHEN $6 THEN COALESCE($9::timestamptz, clock_timestamp()) END,
			        COALESCE($9::timestamptz, clock_timestamp()), COALESCE($9::timestamptz, clock_timestamp()))
			ON CONFLICT (restaurant_id, command_id) DO NOTHING`,
			s.restaurantID, s.orgID, a.CommandID, a.ProposalID, a.Fingerprint, a.Pending, ctxDoc, positions, s.now())
		if isUnique(err, pendingIndexName) {
			return contract.ErrConfirmationPending
		}
		if err != nil {
			return err
		}
		created = tag.RowsAffected() == 1
		row, ok, err := s.readAttempt(ctx, tx, a.CommandID, "")
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrProposalMissing
		}
		if !created {
			return s.compare(row, a, &result, &conflict)
		}
		result, err = row.attempt()
		return err
	})
	if err != nil {
		return contract.ConfirmationAttempt{}, false, err
	}
	if conflict != nil {
		return *conflict, false, contract.ErrCommandIdentityConflict
	}
	return result, created, nil
}

// compare checks a stored attempt against the one a caller wants to begin: the
// same identity is returned in out, a different one in conflict.
func (s *Store) compare(existing attemptRow, want contract.ConfirmationAttempt, out *contract.ConfirmationAttempt, conflict **contract.ConfirmationAttempt) error {
	stored, err := existing.attempt()
	if err != nil {
		return err
	}
	if stored.ProposalID != want.ProposalID || stored.Fingerprint != want.Fingerprint || !sameAccepted(stored.AcceptedPositions, want.AcceptedPositions) {
		*conflict = &stored
		return nil
	}
	*out = stored
	return nil
}

func sameAccepted(left, right []contract.AcceptedPosition) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// lockProposal locks the proposal row of an attempt and checks the lease token
// of the context against it. exclusive picks FOR UPDATE over FOR SHARE. It
// returns the proposal's accepted positions and replacement state.
type lockedProposal struct {
	found      bool
	supersedes *string
	accepted   []domain.AcceptedPosition
}

func (s *Store) lockProposal(ctx context.Context, tx pgx.Tx, proposalID string, exclusive bool) (lockedProposal, error) {
	lock := " FOR SHARE"
	if exclusive {
		lock = " FOR UPDATE"
	}
	var out lockedProposal
	var seq int64
	var accepted []byte
	err := tx.QueryRow(ctx, `SELECT superseded_by, lease_seq, accepted_positions FROM serving_proposals WHERE restaurant_id = $1 AND proposal_id = $2`+lock, s.restaurantID, proposalID).
		Scan(&out.supersedes, &seq, &accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.found = true
	if lease, ok := application.LeaseFromContext(ctx); ok && lease.ProposalID == proposalID && lease.Seq != seq {
		return out, domain.ErrLeaseLost
	}
	var docs []positionDoc
	if err := unmarshal(accepted, &docs); err != nil {
		return out, err
	}
	out.accepted, err = acceptedFromDocs(docs)
	return out, err
}

func (s *Store) ReserveAttempt(ctx context.Context, commandID string) error {
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		head, ok, err := s.readAttempt(ctx, tx, commandID, "")
		if err != nil {
			return err
		}
		if !ok || head.state != string(contract.OrderAttemptPending) {
			return domain.ErrProposalMissing
		}
		proposal, err := s.lockProposal(ctx, tx, head.proposalID, true)
		if err != nil {
			return err
		}
		if !proposal.found {
			return domain.ErrProposalMissing
		}
		if proposal.supersedes != nil {
			return domain.ErrProposalReplaced
		}
		_, err = tx.Exec(ctx, `UPDATE serving_confirmation_attempts
			SET pending = true, next_check_at = COALESCE($3::timestamptz, clock_timestamp()), updated_at = COALESCE($3::timestamptz, clock_timestamp())
			WHERE restaurant_id = $1 AND command_id = $2 AND state = 'Pending' AND NOT pending`, s.restaurantID, commandID, s.now())
		if isUnique(err, pendingIndexName) {
			return contract.ErrConfirmationPending
		}
		return err
	})
}

func (s *Store) RememberWarehouseResult(ctx context.Context, commandID string, result contract.CommitResult, replacement *contract.OrderProposal) error {
	resultDoc, err := marshal(result)
	if err != nil {
		return err
	}
	var replacementDoc []byte
	if replacement != nil {
		if replacementDoc, err = marshal(replacement); err != nil {
			return err
		}
	}
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		head, ok, err := s.readAttempt(ctx, tx, commandID, "")
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrProposalMissing
		}
		if _, err := s.lockProposal(ctx, tx, head.proposalID, false); err != nil {
			return err
		}
		row, ok, err := s.readAttempt(ctx, tx, commandID, " FOR UPDATE")
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrProposalMissing
		}
		same := false
		if row.warehouseResult != nil {
			var stored contract.CommitResult
			if err := unmarshal(row.warehouseResult, &stored); err != nil {
				return err
			}
			same, err = sameJSON(stored, result)
			if err != nil {
				return err
			}
		}
		if row.state != string(contract.OrderAttemptPending) {
			if same {
				return nil
			}
			return contract.ErrCommandIdentityConflict
		}
		if row.warehouseResult != nil && !same {
			return contract.ErrCommandIdentityConflict
		}
		_, err = tx.Exec(ctx, `UPDATE serving_confirmation_attempts
			SET warehouse_result = $3, replacement = COALESCE(replacement, $4::jsonb), updated_at = COALESCE($5::timestamptz, clock_timestamp())
			WHERE restaurant_id = $1 AND command_id = $2`, s.restaurantID, commandID, resultDoc, replacementDoc, s.now())
		return err
	})
}

// sameJSON compares two values by their encoding, which is exact for instants.
func sameJSON(a, b any) (bool, error) {
	left, err := marshal(a)
	if err != nil {
		return false, err
	}
	right, err := marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}

func (s *Store) CompleteAttempt(ctx context.Context, commandID string, result contract.ConfirmResult, rejection *contract.AttemptError, replacement *domain.OrderProposal) error {
	wantState := contract.OrderAttemptCompleted
	var resultDoc, errorDoc []byte
	var err error
	if rejection != nil {
		wantState = contract.OrderAttemptRejected
		if errorDoc, err = marshal(rejection); err != nil {
			return err
		}
	} else if resultDoc, err = marshal(result); err != nil {
		return err
	}
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		head, ok, err := s.readAttempt(ctx, tx, commandID, "")
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrProposalMissing
		}
		proposal, err := s.lockProposal(ctx, tx, head.proposalID, true)
		if err != nil {
			return err
		}
		row, ok, err := s.readAttempt(ctx, tx, commandID, " FOR UPDATE")
		if err != nil {
			return err
		}
		if !ok {
			return domain.ErrProposalMissing
		}
		if row.state != string(contract.OrderAttemptPending) {
			return sameOutcome(row, wantState, resultDoc, rejection)
		}
		if !proposal.found && (row.pending || replacement != nil) {
			return domain.ErrProposalMissing
		}
		replaced := ""
		if row.pending && replacement != nil {
			if replacement.ProposalID == "" || !contract.ValidClientID(replacement.ProposalID) || replacement.ProposalID == head.proposalID || !domain.AcceptedWithin(replacement.AcceptedPositions, proposal.accepted) {
				return domain.ErrInvalidReplace
			}
			next := replacement.Clone()
			next.RequestedPositions = domain.RequestedFromAccepted(proposal.accepted)
			doc, err := marshal(toProposalDoc(next))
			if err != nil {
				return err
			}
			requested, err := marshal(requestedDocs(next.RequestedPositions))
			if err != nil {
				return err
			}
			accepted, err := marshal(acceptedDocs(next.AcceptedPositions))
			if err != nil {
				return err
			}
			changes, err := marshal(changeDocs(next.Changes))
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO serving_proposals
				(restaurant_id, org_id, proposal_id, context, evaluated_at, requested_positions, accepted_positions, changes, position_count, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, COALESCE($10::timestamptz, clock_timestamp()))`,
				s.restaurantID, s.orgID, next.ProposalID, doc, next.EvaluatedAt.UTC(), requested, accepted, changes, len(next.RequestedPositions), s.now())
			if isUnique(err, proposalsPrimaryKey) {
				return domain.ErrProposalExists
			}
			if err != nil {
				return err
			}
			replaced = next.ProposalID
		}
		settle := replaced != "" || (rejection == nil && result.Kind == contract.OrderConfirmed)
		if proposal.found && (replaced != "" || settle) {
			var supersededBy any
			if replaced != "" {
				supersededBy = replaced
			}
			if _, err := tx.Exec(ctx, `UPDATE serving_proposals
				SET superseded_by = COALESCE($3, superseded_by), settled_at = COALESCE(settled_at, COALESCE($4::timestamptz, clock_timestamp()))
				WHERE restaurant_id = $1 AND proposal_id = $2`, s.restaurantID, head.proposalID, supersededBy, s.now()); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE serving_confirmation_attempts
			SET state = $3, pending = false, result = $4, error = $5, warehouse_result = NULL, replacement = NULL, next_check_at = NULL,
			    updated_at = COALESCE($6::timestamptz, clock_timestamp())
			WHERE restaurant_id = $1 AND command_id = $2`, s.restaurantID, commandID, string(wantState), resultDoc, errorDoc, s.now())
		return err
	})
}

// sameOutcome answers a repeated completion of a finished attempt: the same
// outcome is a success without effect, any other is a conflict.
func sameOutcome(row attemptRow, want contract.OrderAttemptState, resultDoc []byte, rejection *contract.AttemptError) error {
	if row.state != string(want) {
		return contract.ErrCommandIdentityConflict
	}
	if rejection != nil {
		var stored contract.AttemptError
		if err := unmarshal(row.errDoc, &stored); err != nil {
			return err
		}
		if stored == *rejection {
			return nil
		}
		return contract.ErrCommandIdentityConflict
	}
	var stored, incoming contract.ConfirmResult
	if err := unmarshal(row.result, &stored); err != nil {
		return err
	}
	if err := unmarshal(resultDoc, &incoming); err != nil {
		return err
	}
	same, err := sameJSON(stored, incoming)
	if err != nil {
		return err
	}
	if same {
		return nil
	}
	return contract.ErrCommandIdentityConflict
}

func (s *Store) AcquireLease(ctx context.Context, proposalID, holder string, ttl time.Duration) (application.Lease, bool, error) {
	if proposalID == "" || holder == "" || ttl <= 0 {
		return application.Lease{}, false, domain.ErrInvalidProposal
	}
	lease := application.Lease{ProposalID: proposalID, Holder: holder}
	granted := false
	err := s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `UPDATE serving_proposals
			SET lease_holder = $3, lease_until = COALESCE($5::timestamptz, clock_timestamp()) + make_interval(secs => $4::float8), lease_seq = lease_seq + 1
			WHERE restaurant_id = $1 AND proposal_id = $2
			  AND (lease_until IS NULL OR lease_until <= COALESCE($5::timestamptz, clock_timestamp()))
			RETURNING lease_seq`, s.restaurantID, proposalID, holder, ttl.Seconds(), s.now()).Scan(&lease.Seq)
		if err == nil {
			granted = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM serving_proposals WHERE restaurant_id = $1 AND proposal_id = $2)`, s.restaurantID, proposalID).Scan(&exists); err != nil {
			return err
		}
		// A proposal that is not there has nothing to serialize.
		granted = !exists
		return nil
	})
	if err != nil {
		return application.Lease{}, false, err
	}
	if !granted {
		return application.Lease{}, false, nil
	}
	return lease, true, nil
}

func (s *Store) ReleaseLease(ctx context.Context, lease application.Lease) error {
	if lease.Seq == 0 {
		return nil
	}
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE serving_proposals SET lease_holder = NULL, lease_until = NULL
			WHERE restaurant_id = $1 AND proposal_id = $2 AND lease_seq = $3 AND lease_holder = $4`, s.restaurantID, lease.ProposalID, lease.Seq, lease.Holder)
		return err
	})
}

func (s *Store) RecordCheck(ctx context.Context, commandID string, delay time.Duration, stuck bool) error {
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE serving_confirmation_attempts
			SET check_count = check_count + 1,
			    next_check_at = COALESCE($5::timestamptz, clock_timestamp()) + make_interval(secs => $3::float8),
			    stuck_at = CASE WHEN $4 AND stuck_at IS NULL THEN COALESCE($5::timestamptz, clock_timestamp()) ELSE stuck_at END,
			    updated_at = COALESCE($5::timestamptz, clock_timestamp())
			WHERE restaurant_id = $1 AND command_id = $2`, s.restaurantID, commandID, delay.Seconds(), stuck, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrProposalMissing
		}
		return nil
	})
}

func (s *Store) MarkUnresolved(ctx context.Context, commandID string) error {
	return s.tx(ctx, true, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE serving_confirmation_attempts
			SET unresolved_at = COALESCE(unresolved_at, COALESCE($3::timestamptz, clock_timestamp())), updated_at = COALESCE($3::timestamptz, clock_timestamp())
			WHERE restaurant_id = $1 AND command_id = $2`, s.restaurantID, commandID, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrProposalMissing
		}
		return nil
	})
}
