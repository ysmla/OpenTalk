-- Search used `LIKE '%q%'`, which cannot use an index: every search scanned every row, and the cost grew
-- with the forum rather than with the answer set. These two full-text indexes replace that scan.
--
-- The ngram parser is what makes this usable for Chinese: it indexes overlapping n-grams (2 characters by
-- default) instead of whitespace-separated words, so a two-character term is an indexed token. That choice
-- has a visible consequence, handled in the query rather than hidden here: a one-character query produces no
-- token at all, so the application falls back to the old scan for those.
--
-- Both indexes are needed because a title lives in `topics` while every body lives in `contents`, and a
-- full-text index cannot span two tables.
ALTER TABLE contents ADD FULLTEXT INDEX ft_contents_body (body_md) WITH PARSER ngram;
ALTER TABLE topics ADD FULLTEXT INDEX ft_topics_title (title) WITH PARSER ngram;
