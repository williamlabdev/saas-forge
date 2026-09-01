-- Reverses 000037. Dropping the table discards every proposal, decided or not:
-- what was asked, by whom, and who answered. The schemas themselves are
-- untouched — an approved proposal was applied through ApplySchema and the
-- result lives in the content types, not here — so what is lost is the record
-- of HOW those changes were authorised, and nothing else records it.
--
-- Pending proposals are lost as work, not just as history: after this runs, an
-- agent that filed a change and is waiting has nothing to wait on and no way to
-- be told.

DROP TABLE IF EXISTS schema_proposals;
