CREATE TABLE IF NOT EXISTS bootstrap_sessions (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    current_step integer NOT NULL DEFAULT 0,
    status text NOT NULL,
    created_by text NOT NULL DEFAULT '',
    data jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS bootstrap_sessions_project_id_idx ON bootstrap_sessions (project_id);
CREATE INDEX IF NOT EXISTS bootstrap_sessions_updated_at_idx ON bootstrap_sessions (updated_at);
