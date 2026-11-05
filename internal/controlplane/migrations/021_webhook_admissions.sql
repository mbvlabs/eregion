-- Durable GitHub push admission. The HTTP handler verifies the signature and
-- writes one sanitized intent plus its River processing job in the same
-- transaction, then replies 202. A worker submits the ordinary platform.deploy
-- operation and links it here in another single transaction, so a crash can
-- never leave an operation without its delivery link or a link without its
-- operation.
--
-- Existing 008 rows stay valid: submitted rows keep their operation ID, and old
-- pending rows have no commit to reconstruct. They are reconciled only when the
-- same delivery is authenticated again; they are never blindly resubmitted.
--
-- This table still stores no raw delivery ID, body, signature, secret, sender or
-- commit message. New columns are hashes, validated payload facts and non-secret
-- environment/revision/secret identities only.
--
-- environment_id and revision_id deliberately have no foreign key: an accepted
-- intent must still be visible after its environment is deleted, so the worker
-- can reject it terminally instead of mistaking a recreated environment for the
-- original one.

ALTER TABLE eregion_webhook_deliveries
    DROP CONSTRAINT IF EXISTS eregion_webhook_deliveries_state_check,
    DROP CONSTRAINT IF EXISTS eregion_webhook_deliveries_check;

ALTER TABLE eregion_webhook_deliveries
    ADD COLUMN environment_id text NOT NULL DEFAULT ''
        CHECK (environment_id = '' OR environment_id ~ '^env_[0-9a-f]{32}$'),
    ADD COLUMN revision_id text NOT NULL DEFAULT ''
        CHECK (revision_id = '' OR revision_id ~ '^erv_[0-9a-f]{32}$'),
    ADD COLUMN secret_revision bigint NOT NULL DEFAULT 0
        CHECK (secret_revision >= 0),
    ADD COLUMN secret_revision_hash text NOT NULL DEFAULT ''
        CHECK (secret_revision_hash = '' OR secret_revision_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN repository text NOT NULL DEFAULT '',
    ADD COLUMN branch text NOT NULL DEFAULT '',
    ADD COLUMN commit_sha text NOT NULL DEFAULT ''
        CHECK (commit_sha = '' OR commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    ADD COLUMN job_id bigint,
    ADD COLUMN admitted_at timestamptz,
    ADD COLUMN failed_at timestamptz,
    ADD COLUMN error_code text NOT NULL DEFAULT '',
    ADD COLUMN error_detail text NOT NULL DEFAULT '',
    ADD COLUMN admission_attempts integer NOT NULL DEFAULT 0
        CHECK (admission_attempts >= 0);

ALTER TABLE eregion_webhook_deliveries
    ADD CONSTRAINT eregion_webhook_deliveries_state_check
    CHECK (state IN ('pending','admitted','submitted','failed'));

ALTER TABLE eregion_webhook_deliveries
    ADD CONSTRAINT eregion_webhook_deliveries_lifecycle_check
    CHECK (
        (state = 'pending' AND operation_id IS NULL AND submitted_at IS NULL)
        OR (state = 'admitted' AND operation_id IS NULL AND submitted_at IS NULL
            AND admitted_at IS NOT NULL AND job_id IS NOT NULL)
        OR (state = 'submitted' AND operation_id IS NOT NULL AND submitted_at IS NOT NULL)
        OR (state = 'failed' AND operation_id IS NULL AND submitted_at IS NULL
            AND failed_at IS NOT NULL AND error_code <> '')
    );

-- The maintenance reconciler finds admitted receipts whose River job vanished
-- or reached a terminal state without committing the operation.
CREATE INDEX eregion_webhook_deliveries_admitted
    ON eregion_webhook_deliveries (job_id)
    WHERE state = 'admitted';
