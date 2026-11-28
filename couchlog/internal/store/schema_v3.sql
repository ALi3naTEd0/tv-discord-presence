-- Version 3: an item can be a guess (the episode after your last, when the
-- app does not say which): it is never sent until you confirm it.
ALTER TABLE items ADD COLUMN guessed INTEGER NOT NULL DEFAULT 0;
