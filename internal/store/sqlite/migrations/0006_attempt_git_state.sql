ALTER TABLE attempts ADD COLUMN git_state_json BLOB
    CHECK (git_state_json IS NULL OR json_valid(git_state_json));
