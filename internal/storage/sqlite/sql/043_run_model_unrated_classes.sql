-- A NULL unrated_classes marks a part whose classes were not recorded, an empty string a part with none.

ALTER TABLE run_model_usage ADD COLUMN unrated_classes TEXT;
