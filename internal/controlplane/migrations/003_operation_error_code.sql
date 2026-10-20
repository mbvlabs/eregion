ALTER TABLE eregion_operations
    ADD COLUMN error_code text NOT NULL DEFAULT ''
    CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error'));

ALTER TABLE eregion_operation_steps
    ADD COLUMN error_code text NOT NULL DEFAULT ''
    CHECK (error_code IN ('', 'usage_error', 'invalid_output_mode', 'input_required', 'validation_failed',
                           'not_found', 'unauthenticated', 'forbidden', 'precondition_failed',
                           'confirmation_required', 'conflict', 'unavailable', 'cancelled', 'internal_error'));
