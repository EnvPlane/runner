CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS environments (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    pr_id text NOT NULL DEFAULT '',
    branch text NOT NULL DEFAULT '',
    commit_sha text NOT NULL DEFAULT '',
    status text NOT NULL,
    type text NOT NULL,
    ttl integer NOT NULL DEFAULT 0,
    payload jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS environments_project_id_idx ON environments (project_id);
CREATE INDEX IF NOT EXISTS environments_status_idx ON environments (status);
CREATE INDEX IF NOT EXISTS environments_pr_id_idx ON environments (pr_id);

CREATE TABLE IF NOT EXISTS projects (
    id text PRIMARY KEY,
    payload jsonb NOT NULL,
    product_id text NOT NULL DEFAULT '',
    app_repository_id text NOT NULL DEFAULT '',
    gitops_repository_id text NOT NULL DEFAULT '',
    cluster_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS projects_product_id_idx ON projects (product_id);
CREATE INDEX IF NOT EXISTS projects_app_repository_id_idx ON projects (app_repository_id);

CREATE TABLE IF NOT EXISTS products (
    name text PRIMARY KEY,
    payload jsonb NOT NULL,
    manifest_source_id text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS products_manifest_source_id_idx ON products (manifest_source_id);

CREATE TABLE IF NOT EXISTS control_plane_settings (
    id text PRIMARY KEY DEFAULT 'default',
    payload jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT control_plane_settings_singleton CHECK (id = 'default')
);

CREATE TABLE IF NOT EXISTS jobs (
    id text PRIMARY KEY,
    type text NOT NULL,
    status text NOT NULL,
    environment_id text NOT NULL DEFAULT '',
    event jsonb NOT NULL,
    request jsonb NOT NULL,
    result jsonb,
    error text NOT NULL DEFAULT '',
    attempts integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 0,
    next_run_at timestamptz,
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz
);

CREATE INDEX IF NOT EXISTS jobs_status_idx ON jobs (status);
CREATE INDEX IF NOT EXISTS jobs_environment_id_idx ON jobs (environment_id);
CREATE INDEX IF NOT EXISTS jobs_next_run_at_idx ON jobs (next_run_at);
