-- Dropping the indexes returns search to the previous behaviour (the query falls back to LIKE when the
-- full-text indexes are absent only if it is short; a normal query would fail), so this down migration is
-- meant for a rollback of the whole change, not for running the new code against the old schema.
DROP INDEX ft_topics_title ON topics;
DROP INDEX ft_contents_body ON contents;
