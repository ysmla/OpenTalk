-- Destructive rollback for a disposable development database only.
ALTER TABLE settings
  DROP CONSTRAINT ck_settings_media_orphan_days,
  DROP CONSTRAINT ck_settings_media_pending_ttl,
  DROP CONSTRAINT ck_settings_media_count,
  DROP CONSTRAINT ck_settings_media_dimension,
  DROP CONSTRAINT ck_settings_media_bytes;

ALTER TABLE settings
  DROP COLUMN media_orphan_cleanup_days,
  DROP COLUMN media_pending_ttl_hours,
  DROP COLUMN media_max_per_content,
  DROP COLUMN media_max_dimension,
  DROP COLUMN media_max_bytes;
