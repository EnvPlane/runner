CREATE TABLE IF NOT EXISTS project_config_versions (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    version integer NOT NULL,
    config jsonb NOT NULL,
    sensitive jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_by text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_config_versions_project_version_unique UNIQUE (project_id, version)
);

CREATE INDEX IF NOT EXISTS project_config_versions_project_id_idx ON project_config_versions (project_id);
CREATE INDEX IF NOT EXISTS project_config_versions_created_at_idx ON project_config_versions (created_at);
