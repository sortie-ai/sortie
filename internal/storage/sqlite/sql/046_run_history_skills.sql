-- NULL marks a row written before skill calls were kept, or a marshal failure, and [] marks a run that called none.

ALTER TABLE run_history ADD COLUMN skills TEXT;
