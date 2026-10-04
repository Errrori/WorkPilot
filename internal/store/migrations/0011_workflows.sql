create table if not exists workflows (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    name text not null,
    description text not null default '',
    definition jsonb not null,
    enabled boolean not null default true,
    schedule text not null default '',
    timezone text not null default 'Asia/Shanghai',
    created_by text not null default '',
    last_run_at timestamptz,
    last_status text,
    last_error text not null default '',
    next_run_at timestamptz not null default now(),
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table workflows drop constraint if exists workflows_last_status_check;
alter table workflows add constraint workflows_last_status_check
    check (last_status is null or last_status in ('succeeded', 'failed'));

create index if not exists idx_workflows_due on workflows (next_run_at) where enabled and schedule <> '';

create table if not exists workflow_runs (
    id bigserial primary key,
    workflow_id bigint not null references workflows(id) on delete cascade,
    group_id uuid not null references groups(id) on delete cascade,
    status text not null default 'running',
    trigger text not null default 'manual',
    error text not null default '',
    started_at timestamptz not null default now(),
    finished_at timestamptz
);

alter table workflow_runs drop constraint if exists workflow_runs_status_check;
alter table workflow_runs add constraint workflow_runs_status_check
    check (status in ('running', 'succeeded', 'failed'));

alter table workflow_runs drop constraint if exists workflow_runs_trigger_check;
alter table workflow_runs add constraint workflow_runs_trigger_check
    check (trigger in ('manual', 'schedule'));

create index if not exists idx_workflow_runs_workflow on workflow_runs (workflow_id, id desc);
create index if not exists idx_workflow_runs_group on workflow_runs (group_id, id desc);

create table if not exists workflow_node_runs (
    id bigserial primary key,
    run_id bigint not null references workflow_runs(id) on delete cascade,
    node_id text not null,
    type text not null,
    status text not null default 'pending',
    input jsonb,
    output jsonb,
    error text not null default '',
    tokens int not null default 0,
    latency_ms int not null default 0,
    started_at timestamptz,
    finished_at timestamptz
);

alter table workflow_node_runs drop constraint if exists workflow_node_runs_status_check;
alter table workflow_node_runs add constraint workflow_node_runs_status_check
    check (status in ('pending', 'running', 'succeeded', 'failed', 'skipped'));

create unique index if not exists idx_workflow_node_runs_node on workflow_node_runs (run_id, node_id);
