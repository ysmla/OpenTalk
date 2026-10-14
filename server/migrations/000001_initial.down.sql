-- Destructive rollback for a disposable development database only.
ALTER TABLE notifications DROP FOREIGN KEY fk_notifications_action;
ALTER TABLE contents DROP FOREIGN KEY fk_contents_anon_author;
ALTER TABLE users DROP FOREIGN KEY fk_users_avatar;
DROP TABLE deleted_content_archives;
DROP TABLE moderation_actions;
DROP TABLE user_restrictions;
DROP TABLE reports;
DROP TABLE email_deliveries;
DROP TABLE notifications;
DROP TABLE outbox_events;
DROP TABLE topic_subscriptions;
DROP TABLE notification_preferences;
DROP TABLE content_attachments;
DROP TABLE attachments;
DROP TABLE topic_tags;
DROP TABLE tags;
DROP TABLE anonymous_identities;
DROP TABLE comments;
DROP TABLE answers;
DROP TABLE questions;
DROP TABLE topics;
DROP TABLE contents;
DROP TABLE boards;
DROP TABLE sessions;
DROP TABLE users;

