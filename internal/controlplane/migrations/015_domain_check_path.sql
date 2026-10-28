-- D29: each domain binding may carry its own check path. Empty means the
-- environment's web health path, preserving existing bindings.

ALTER TABLE eregion_platform_domains
    ADD COLUMN check_path text NOT NULL DEFAULT ''
        CHECK (length(check_path) <= 256);
