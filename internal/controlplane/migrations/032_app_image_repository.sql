-- App-level private image repository (beta.5).
--
-- The repository is immutable app metadata: one exact private workspace path
-- per app, unique across the platform. It is never an environment recipe or
-- override. Existing apps from before this contract keep an empty value and are
-- refused clearly when an artifact or build needs their repository; there is no
-- historical backfill that guesses one.
ALTER TABLE eregion_platform_apps
    ADD COLUMN image_repository text NOT NULL DEFAULT ''
        CHECK (length(image_repository) <= 200);

CREATE UNIQUE INDEX eregion_platform_apps_image_repository_unique
    ON eregion_platform_apps (image_repository)
    WHERE image_repository <> '';
