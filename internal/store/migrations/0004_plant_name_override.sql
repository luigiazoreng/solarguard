-- Keep a human correction separate from the provider's discovery name.
ALTER TABLE plants ADD COLUMN name_override TEXT;
