-- Serving storage (serving.md 5.2). Applied by the migrate job under the owner
-- role umir_serving_owner. The runtime role gets its privileges from the roles
-- job (default privileges), not from this file.

-- umir:include storage_meta

-- Registrations received from Auth (PUT/DELETE registry). A row is created by
-- the first register or by a close that arrives before any registration; a
-- closed row stays as a tombstone without time limit.
CREATE TABLE serving_restaurants (
    restaurant_id            uuid        PRIMARY KEY,
    org_id                   uuid        NOT NULL,
    lifecycle_version        bigint      NOT NULL CHECK (lifecycle_version > 0),
    time_zone                text,
    settings_version         bigint      CHECK (settings_version > 0),
    operation_id             uuid        NOT NULL,
    state                    text        NOT NULL CHECK (state IN ('registered', 'closed')),
    close_operation_id       uuid,
    close_lifecycle_version  bigint      CHECK (close_lifecycle_version > 0),
    registered_at            timestamptz NOT NULL,
    closed_at                timestamptz,
    updated_at               timestamptz NOT NULL,
    UNIQUE (restaurant_id, org_id),
    CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
    CHECK ((state = 'closed') = (close_operation_id IS NOT NULL)),
    CHECK ((state = 'closed') = (close_lifecycle_version IS NOT NULL)),
    CHECK (state = 'closed' OR (time_zone IS NOT NULL AND settings_version IS NOT NULL))
);

CREATE TABLE serving_proposals (
    restaurant_id       uuid        NOT NULL,
    org_id              uuid        NOT NULL,
    proposal_id         text        NOT NULL CHECK (char_length(proposal_id) BETWEEN 1 AND 256),
    context             jsonb       NOT NULL,
    evaluated_at        timestamptz NOT NULL,
    requested_positions jsonb       NOT NULL,
    accepted_positions  jsonb       NOT NULL,
    changes             jsonb       NOT NULL,
    position_count      int         NOT NULL CHECK (position_count >= 0),
    superseded_by       text,
    settled_at          timestamptz,
    lease_holder        text,
    lease_until         timestamptz,
    lease_seq           bigint      NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (restaurant_id, proposal_id),
    FOREIGN KEY (restaurant_id, org_id) REFERENCES serving_restaurants (restaurant_id, org_id)
);
CREATE INDEX serving_proposals_active ON serving_proposals (restaurant_id) WHERE settled_at IS NULL;
CREATE INDEX serving_proposals_evaluated ON serving_proposals (evaluated_at);

CREATE TABLE serving_confirmation_attempts (
    restaurant_id      uuid        NOT NULL,
    org_id             uuid        NOT NULL,
    command_id         text        NOT NULL CHECK (char_length(command_id) BETWEEN 1 AND 256),
    proposal_id        text        NOT NULL CHECK (char_length(proposal_id) BETWEEN 1 AND 256),
    fingerprint        text        NOT NULL,
    state              text        NOT NULL CHECK (state IN ('Pending', 'Completed', 'Rejected')),
    pending            boolean     NOT NULL DEFAULT false CHECK (NOT pending OR state = 'Pending'),
    context            jsonb       NOT NULL,
    accepted_positions jsonb       NOT NULL,
    warehouse_result   jsonb,
    replacement        jsonb,
    result             jsonb,
    error              jsonb,
    check_count        int         NOT NULL DEFAULT 0,
    next_check_at      timestamptz,
    stuck_at           timestamptz,
    unresolved_at      timestamptz,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (restaurant_id, command_id),
    CHECK ((state = 'Completed') = (result IS NOT NULL)),
    CHECK ((state = 'Rejected') = (error IS NOT NULL)),
    FOREIGN KEY (restaurant_id, org_id) REFERENCES serving_restaurants (restaurant_id, org_id)
);
-- At most one command owns a proposal (the reservation made before a commit is sent).
CREATE UNIQUE INDEX serving_attempts_one_pending ON serving_confirmation_attempts (restaurant_id, proposal_id) WHERE pending;
CREATE INDEX serving_attempts_proposal ON serving_confirmation_attempts (restaurant_id, proposal_id);
CREATE INDEX serving_attempts_due ON serving_confirmation_attempts (next_check_at) WHERE pending AND unresolved_at IS NULL;
CREATE INDEX serving_attempts_settled ON serving_confirmation_attempts (updated_at) WHERE NOT pending;

-- Row level security (R8): a transaction sees the rows of its restaurant, or all
-- rows when it is a system transaction (sweeper, reconciler pick).
ALTER TABLE "serving_restaurants" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "serving_restaurants" FORCE ROW LEVEL SECURITY;
CREATE POLICY "serving_restaurants_scope_restaurant_id" ON "serving_restaurants"
    USING (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid)
    WITH CHECK (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid);
ALTER TABLE "serving_proposals" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "serving_proposals" FORCE ROW LEVEL SECURITY;
CREATE POLICY "serving_proposals_scope_restaurant_id" ON "serving_proposals"
    USING (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid)
    WITH CHECK (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid);
ALTER TABLE "serving_confirmation_attempts" ENABLE ROW LEVEL SECURITY;
ALTER TABLE "serving_confirmation_attempts" FORCE ROW LEVEL SECURITY;
CREATE POLICY "serving_confirmation_attempts_scope_restaurant_id" ON "serving_confirmation_attempts"
    USING (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid)
    WITH CHECK (NULLIF(current_setting('umir.actor', true), '') = 'system' OR "restaurant_id" = NULLIF(current_setting('umir.restaurant_id', true), '')::uuid);
