-- Site settings live in a single row so "the current configuration" is unambiguous.
-- The SMTP password has no plaintext column: only application-level AES-GCM ciphertext is stored.

CREATE TABLE settings (
  id TINYINT UNSIGNED NOT NULL PRIMARY KEY,
  smtp_host VARCHAR(255) CHARACTER SET ascii NULL,
  smtp_port SMALLINT UNSIGNED NULL,
  smtp_security ENUM('none','starttls','tls') NOT NULL DEFAULT 'starttls',
  smtp_username VARCHAR(255) NULL,
  smtp_password_ciphertext VARBINARY(512) NULL,
  smtp_from_address VARCHAR(254) NULL,
  smtp_from_name VARCHAR(120) NOT NULL DEFAULT '',
  captcha_ttl_seconds INT UNSIGNED NOT NULL DEFAULT 300,
  captcha_max_attempts TINYINT UNSIGNED NOT NULL DEFAULT 3,
  email_code_ttl_seconds INT UNSIGNED NOT NULL DEFAULT 600,
  email_code_max_attempts TINYINT UNSIGNED NOT NULL DEFAULT 5,
  email_code_resend_interval_seconds INT UNSIGNED NOT NULL DEFAULT 60,
  email_code_email_hourly_limit SMALLINT UNSIGNED NOT NULL DEFAULT 5,
  email_code_ip_hourly_limit SMALLINT UNSIGNED NOT NULL DEFAULT 20,
  login_captcha_failure_threshold TINYINT UNSIGNED NOT NULL DEFAULT 5,
  updated_by BIGINT UNSIGNED NULL,
  updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  CONSTRAINT ck_settings_singleton CHECK (id = 1),
  CONSTRAINT ck_settings_smtp_port CHECK (smtp_port IS NULL OR smtp_port > 0),
  CONSTRAINT ck_settings_captcha_ttl CHECK (captcha_ttl_seconds BETWEEN 60 AND 1800),
  CONSTRAINT ck_settings_captcha_attempts CHECK (captcha_max_attempts BETWEEN 1 AND 10),
  CONSTRAINT ck_settings_email_ttl CHECK (email_code_ttl_seconds BETWEEN 60 AND 3600),
  CONSTRAINT ck_settings_email_attempts CHECK (email_code_max_attempts BETWEEN 1 AND 10),
  CONSTRAINT ck_settings_resend CHECK (email_code_resend_interval_seconds BETWEEN 10 AND 600),
  CONSTRAINT ck_settings_email_limit CHECK (email_code_email_hourly_limit BETWEEN 1 AND 100),
  CONSTRAINT ck_settings_ip_limit CHECK (email_code_ip_hourly_limit BETWEEN 1 AND 1000),
  CONSTRAINT ck_settings_login_threshold CHECK (login_captcha_failure_threshold BETWEEN 1 AND 50),
  CONSTRAINT fk_settings_updated_by FOREIGN KEY (updated_by) REFERENCES users (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- SMTP starts unconfigured on purpose: only an operator may set credentials, through the CLI or
-- the admin surface. Seeding a host here would make the application attempt real deliveries.
INSERT INTO settings (id) VALUES (1);
