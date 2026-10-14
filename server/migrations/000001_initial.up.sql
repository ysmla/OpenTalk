-- OpenTalk initial schema, MySQL 8.4. All times are UTC.
-- Business deletion is soft deletion. Default FK action is RESTRICT.
-- CHECK + FK columns intentionally omit explicit referential actions.

CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  public_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  email_normalized VARCHAR(254) COLLATE utf8mb4_0900_bin NOT NULL,
  password_hash VARCHAR(255) CHARACTER SET ascii NOT NULL,
  nickname VARCHAR(80) NOT NULL,
  bio TEXT NOT NULL,
  avatar_attachment_id BIGINT UNSIGNED NULL,
  role ENUM('user','admin') NOT NULL DEFAULT 'user',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_users_public (public_id),
  UNIQUE KEY uq_users_email (email_normalized)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE sessions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  token_hash BINARY(32) NOT NULL,
  csrf_token_hash BINARY(32) NOT NULL,
  expires_at DATETIME(6) NOT NULL,
  revoked_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_sessions_token (token_hash),
  KEY ix_sessions_user_expiry (user_id, expires_at),
  KEY ix_sessions_expiry (expires_at),
  CONSTRAINT fk_sessions_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE boards (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  description TEXT NOT NULL,
  kind ENUM('discussion','qa') NOT NULL,
  sort_order INT UNSIGNED NOT NULL DEFAULT 0,
  is_archived BOOLEAN NOT NULL DEFAULT FALSE,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_boards_archived CHECK (is_archived IN (0,1)),
  KEY ix_boards_sort (sort_order,id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE contents (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  author_id BIGINT UNSIGNED NOT NULL,
  kind ENUM('topic','answer','comment') NOT NULL,
  body_md MEDIUMTEXT NULL,
  is_anonymous BOOLEAN NOT NULL,
  anonymous_identity_id BIGINT UNSIGNED NULL,
  moderation_status ENUM('visible','hidden') NOT NULL DEFAULT 'visible',
  deleted_at DATETIME(6) NULL,
  edited_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_contents_anonymous CHECK (is_anonymous IN (0,1)),
  CONSTRAINT ck_contents_body CHECK ((deleted_at IS NULL AND body_md IS NOT NULL) OR (deleted_at IS NOT NULL AND body_md IS NULL)),
  KEY ix_contents_author_created (author_id,created_at,id),
  KEY ix_contents_created (created_at,id),
  CONSTRAINT fk_contents_author FOREIGN KEY (author_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE topics (
  content_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  board_id BIGINT UNSIGNED NOT NULL,
  title VARCHAR(240) NULL,
  is_locked BOOLEAN NOT NULL DEFAULT FALSE,
  is_pinned BOOLEAN NOT NULL DEFAULT FALSE,
  last_activity_at DATETIME(6) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_topics_flags CHECK (is_locked IN (0,1) AND is_pinned IN (0,1)),
  KEY ix_topics_board_active (board_id,is_pinned,last_activity_at,content_id),
  KEY ix_topics_board_created (board_id,created_at,content_id),
  CONSTRAINT fk_topics_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_topics_board FOREIGN KEY (board_id) REFERENCES boards (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE questions (
  topic_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  resolution_status ENUM('open','solved') NOT NULL DEFAULT 'open',
  resolved_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_questions_resolution CHECK ((resolution_status='open' AND resolved_at IS NULL) OR (resolution_status='solved' AND resolved_at IS NOT NULL)),
  KEY ix_questions_status (resolution_status,topic_id),
  CONSTRAINT fk_questions_topic FOREIGN KEY (topic_id) REFERENCES topics (content_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE answers (
  content_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  question_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_answers_content_question (content_id,question_id),
  KEY ix_answers_question (question_id,content_id),
  CONSTRAINT fk_answers_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_answers_question FOREIGN KEY (question_id) REFERENCES questions (topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE comments (
  content_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  topic_id BIGINT UNSIGNED NOT NULL,
  answer_id BIGINT UNSIGNED NULL,
  parent_comment_id BIGINT UNSIGNED NULL,
  reply_to_comment_id BIGINT UNSIGNED NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_comments_content_topic (content_id,topic_id),
  KEY ix_comments_thread (topic_id,answer_id,parent_comment_id,content_id),
  KEY ix_comments_reply (reply_to_comment_id,content_id),
  CONSTRAINT ck_comments_parents CHECK ((parent_comment_id IS NULL AND reply_to_comment_id IS NULL) OR (parent_comment_id IS NOT NULL AND reply_to_comment_id IS NOT NULL)),
  CONSTRAINT ck_comments_self CHECK ((parent_comment_id IS NULL OR parent_comment_id <> content_id) AND (reply_to_comment_id IS NULL OR reply_to_comment_id <> content_id)),
  CONSTRAINT fk_comments_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_comments_topic FOREIGN KEY (topic_id) REFERENCES topics (content_id),
  CONSTRAINT fk_comments_answer_topic FOREIGN KEY (answer_id,topic_id) REFERENCES answers (content_id,question_id),
  CONSTRAINT fk_comments_parent_topic FOREIGN KEY (parent_comment_id,topic_id) REFERENCES comments (content_id,topic_id),
  CONSTRAINT fk_comments_reply_topic FOREIGN KEY (reply_to_comment_id,topic_id) REFERENCES comments (content_id,topic_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE anonymous_identities (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  topic_id BIGINT UNSIGNED NOT NULL,
  user_id BIGINT UNSIGNED NOT NULL,
  display_number INT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_anon_topic_user (topic_id,user_id),
  UNIQUE KEY uq_anon_topic_number (topic_id,display_number),
  UNIQUE KEY uq_anon_id_user (id,user_id),
  CONSTRAINT ck_anon_number CHECK (display_number > 0),
  CONSTRAINT fk_anon_topic FOREIGN KEY (topic_id) REFERENCES topics (content_id),
  CONSTRAINT fk_anon_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE tags (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(80) NOT NULL,
  normalized_name VARCHAR(80) COLLATE utf8mb4_0900_bin NOT NULL,
  description TEXT NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_tags_name (normalized_name)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE topic_tags (
  topic_id BIGINT UNSIGNED NOT NULL,
  tag_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (topic_id,tag_id),
  KEY ix_topic_tags_reverse (tag_id,topic_id),
  CONSTRAINT fk_topic_tags_topic FOREIGN KEY (topic_id) REFERENCES topics (content_id),
  CONSTRAINT fk_topic_tags_tag FOREIGN KEY (tag_id) REFERENCES tags (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE attachments (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  owner_id BIGINT UNSIGNED NOT NULL,
  storage_key VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  mime_type VARCHAR(100) CHARACTER SET ascii NOT NULL,
  byte_size BIGINT UNSIGNED NOT NULL,
  status ENUM('pending','active','pending_delete','deleted') NOT NULL DEFAULT 'pending',
  delete_after DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_attachments_key (storage_key),
  KEY ix_attachments_owner (owner_id,id),
  KEY ix_attachments_cleanup (status,delete_after,id),
  CONSTRAINT fk_attachments_owner FOREIGN KEY (owner_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE content_attachments (
  content_id BIGINT UNSIGNED NOT NULL,
  attachment_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  PRIMARY KEY (content_id,attachment_id),
  KEY ix_content_attachments_reverse (attachment_id,content_id),
  CONSTRAINT fk_content_attachments_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_content_attachments_attachment FOREIGN KEY (attachment_id) REFERENCES attachments (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE notification_preferences (
  user_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  email_on_comment BOOLEAN NOT NULL,
  email_on_answer BOOLEAN NOT NULL,
  email_on_reply BOOLEAN NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_preferences_flags CHECK (email_on_comment IN (0,1) AND email_on_answer IN (0,1) AND email_on_reply IN (0,1)),
  CONSTRAINT fk_preferences_user FOREIGN KEY (user_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE topic_subscriptions (
  user_id BIGINT UNSIGNED NOT NULL,
  topic_id BIGINT UNSIGNED NOT NULL,
  email_override ENUM('inherit','enabled','disabled') NOT NULL DEFAULT 'inherit',
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (user_id,topic_id),
  CONSTRAINT fk_subscriptions_user FOREIGN KEY (user_id) REFERENCES users (id),
  CONSTRAINT fk_subscriptions_topic FOREIGN KEY (topic_id) REFERENCES topics (content_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE outbox_events (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  event_key VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_type VARCHAR(60) CHARACTER SET ascii NOT NULL,
  content_id BIGINT UNSIGNED NULL,
  recipient_id BIGINT UNSIGNED NULL,
  status ENUM('pending','processing','done','failed') NOT NULL DEFAULT 'pending',
  available_at DATETIME(6) NOT NULL,
  lease_until DATETIME(6) NULL,
  lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_outbox_key (event_key),
  KEY ix_outbox_work (status,available_at,id),
  CONSTRAINT fk_outbox_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_outbox_recipient FOREIGN KEY (recipient_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE notifications (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  recipient_id BIGINT UNSIGNED NOT NULL,
  event_key VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  event_type ENUM('new_comment','new_answer','new_reply','moderation_result') NOT NULL,
  content_id BIGINT UNSIGNED NULL,
  moderation_action_id BIGINT UNSIGNED NULL,
  read_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_notifications_event (recipient_id,event_key),
  KEY ix_notifications_unread (recipient_id,read_at,id),
  CONSTRAINT fk_notifications_recipient FOREIGN KEY (recipient_id) REFERENCES users (id),
  CONSTRAINT fk_notifications_content FOREIGN KEY (content_id) REFERENCES contents (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE email_deliveries (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  recipient_id BIGINT UNSIGNED NOT NULL,
  event_key VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  purpose VARCHAR(40) CHARACTER SET ascii NOT NULL,
  content_id BIGINT UNSIGNED NULL,
  status ENUM('pending','processing','sent','cancelled','failed') NOT NULL DEFAULT 'pending',
  next_attempt_at DATETIME(6) NOT NULL,
  lease_until DATETIME(6) NULL,
  lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  sent_at DATETIME(6) NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_email_event (recipient_id,event_key,purpose),
  KEY ix_email_work (status,next_attempt_at,id),
  CONSTRAINT fk_email_recipient FOREIGN KEY (recipient_id) REFERENCES users (id),
  CONSTRAINT fk_email_content FOREIGN KEY (content_id) REFERENCES contents (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE reports (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  reporter_id BIGINT UNSIGNED NOT NULL,
  content_id BIGINT UNSIGNED NOT NULL,
  reason VARCHAR(500) NOT NULL,
  details TEXT NOT NULL,
  status ENUM('pending','processing','resolved','dismissed') NOT NULL DEFAULT 'pending',
  handler_id BIGINT UNSIGNED NULL,
  resolution_note TEXT NULL,
  resolved_at DATETIME(6) NULL,
  active_slot TINYINT GENERATED ALWAYS AS (CASE WHEN status IN ('pending','processing') THEN 1 ELSE NULL END) STORED,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  UNIQUE KEY uq_reports_active (reporter_id,content_id,active_slot),
  KEY ix_reports_work (status,created_at,id),
  CONSTRAINT ck_reports_resolution CHECK ((status IN ('pending','processing') AND resolved_at IS NULL) OR (status IN ('resolved','dismissed') AND resolved_at IS NOT NULL)),
  CONSTRAINT fk_reports_user FOREIGN KEY (reporter_id) REFERENCES users (id),
  CONSTRAINT fk_reports_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_reports_handler FOREIGN KEY (handler_id) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE user_restrictions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  kind ENUM('mute','ban') NOT NULL,
  reason VARCHAR(1000) NOT NULL,
  starts_at DATETIME(6) NOT NULL,
  ends_at DATETIME(6) NULL,
  revoked_at DATETIME(6) NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  KEY ix_restrictions_effective (user_id,kind,revoked_at,ends_at),
  CONSTRAINT ck_restrictions_time CHECK (ends_at IS NULL OR ends_at > starts_at),
  CONSTRAINT fk_restrictions_user FOREIGN KEY (user_id) REFERENCES users (id),
  CONSTRAINT fk_restrictions_admin FOREIGN KEY (created_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE moderation_actions (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  actor_id BIGINT UNSIGNED NOT NULL,
  target_content_id BIGINT UNSIGNED NULL,
  target_user_id BIGINT UNSIGNED NULL,
  action VARCHAR(60) CHARACTER SET ascii NOT NULL,
  reason TEXT NOT NULL,
  report_id BIGINT UNSIGNED NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  KEY ix_actions_content (target_content_id,id),
  KEY ix_actions_user (target_user_id,id),
  CONSTRAINT fk_actions_actor FOREIGN KEY (actor_id) REFERENCES users (id),
  CONSTRAINT fk_actions_content FOREIGN KEY (target_content_id) REFERENCES contents (id),
  CONSTRAINT fk_actions_user FOREIGN KEY (target_user_id) REFERENCES users (id),
  CONSTRAINT fk_actions_report FOREIGN KEY (report_id) REFERENCES reports (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE deleted_content_archives (
  content_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  original_title VARCHAR(240) NULL,
  original_body_md MEDIUMTEXT NOT NULL,
  deleted_by BIGINT UNSIGNED NOT NULL,
  deletion_root_id BIGINT UNSIGNED NOT NULL,
  deleted_at DATETIME(6) NOT NULL,
  purge_after DATETIME(6) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  KEY ix_archives_purge (purge_after,content_id),
  CONSTRAINT ck_archives_retention CHECK (purge_after = DATE_ADD(deleted_at, INTERVAL 30 DAY)),
  CONSTRAINT fk_archives_content FOREIGN KEY (content_id) REFERENCES contents (id),
  CONSTRAINT fk_archives_actor FOREIGN KEY (deleted_by) REFERENCES users (id),
  CONSTRAINT fk_archives_root FOREIGN KEY (deletion_root_id) REFERENCES contents (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE users ADD CONSTRAINT fk_users_avatar FOREIGN KEY (avatar_attachment_id) REFERENCES attachments(id);
ALTER TABLE contents ADD CONSTRAINT fk_contents_anon_author FOREIGN KEY (anonymous_identity_id,author_id) REFERENCES anonymous_identities(id,user_id);
ALTER TABLE notifications ADD CONSTRAINT fk_notifications_action FOREIGN KEY (moderation_action_id) REFERENCES moderation_actions(id);

