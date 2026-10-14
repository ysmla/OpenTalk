-- Image limits and cleanup windows join the account parameters in the same single row, so an
-- administrator can adjust them from one place. Every column is NOT NULL with a default, so the
-- existing row is populated without a data migration.

ALTER TABLE settings
  ADD COLUMN media_max_bytes BIGINT UNSIGNED NOT NULL DEFAULT 5242880,
  ADD COLUMN media_max_dimension SMALLINT UNSIGNED NOT NULL DEFAULT 4096,
  ADD COLUMN media_max_per_content TINYINT UNSIGNED NOT NULL DEFAULT 9,
  ADD COLUMN media_pending_ttl_hours SMALLINT UNSIGNED NOT NULL DEFAULT 24,
  ADD COLUMN media_orphan_cleanup_days SMALLINT UNSIGNED NOT NULL DEFAULT 7;

-- Bounds are checked here as well as in the application so a direct SQL edit cannot store a value
-- that makes uploads impossible or leaves orphans forever.
ALTER TABLE settings
  ADD CONSTRAINT ck_settings_media_bytes CHECK (media_max_bytes BETWEEN 65536 AND 104857600),
  ADD CONSTRAINT ck_settings_media_dimension CHECK (media_max_dimension BETWEEN 256 AND 8192),
  ADD CONSTRAINT ck_settings_media_count CHECK (media_max_per_content BETWEEN 1 AND 30),
  ADD CONSTRAINT ck_settings_media_pending_ttl CHECK (media_pending_ttl_hours BETWEEN 1 AND 720),
  ADD CONSTRAINT ck_settings_media_orphan_days CHECK (media_orphan_cleanup_days BETWEEN 1 AND 90);
