-- Cut over only with old webhook admission and workers paused. The locks keep
-- a job from starting while its recorded receipt and arguments are inspected.
LOCK TABLE eregion_webhook_deliveries, river_job IN SHARE ROW EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM river_job WHERE kind='webhook_admission' AND state='running') THEN
        RAISE EXCEPTION USING ERRCODE='ER029', MESSAGE='webhook identity cutover blocked: running webhook jobs exist';
    END IF;
    IF EXISTS (SELECT 1 FROM eregion_webhook_deliveries
               WHERE environment_id='' AND state IN ('pending','admitted')) THEN
        RAISE EXCEPTION USING ERRCODE='ER029', MESSAGE='webhook identity cutover blocked: unresolved legacy receipts lack environment identity';
    END IF;
    IF EXISTS (
        SELECT 1 FROM river_job j
        LEFT JOIN eregion_webhook_deliveries d ON d.job_id=j.id
        WHERE j.kind='webhook_admission' AND j.state IN ('available','pending','scheduled','retryable')
          AND (d.job_id IS NULL OR d.environment_id='' OR j.args->>'delivery_id_hash' IS DISTINCT FROM d.delivery_id_hash
               OR (j.args ? 'environment_id' AND j.args->>'environment_id' IS DISTINCT FROM d.environment_id))
    ) THEN
        RAISE EXCEPTION USING ERRCODE='ER029', MESSAGE='webhook identity cutover blocked: queued webhook job has no matching scoped receipt';
    END IF;
END $$;

UPDATE river_job j
SET args=jsonb_set(j.args, '{environment_id}', to_jsonb(d.environment_id), true)
FROM eregion_webhook_deliveries d
WHERE j.id=d.job_id AND j.kind='webhook_admission' AND d.environment_id<>''
  AND j.args->>'delivery_id_hash'=d.delivery_id_hash;

ALTER TABLE eregion_webhook_deliveries
    DROP CONSTRAINT eregion_webhook_deliveries_pkey,
    ADD PRIMARY KEY (environment_id, delivery_id_hash);
