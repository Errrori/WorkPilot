create table if not exists ai_tasks (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    name text not null,
    prompt text not null default '',
    schedule text not null,
    timezone text not null default 'Asia/Shanghai',
    sources jsonb not null default '["messages", "tasks", "risks", "files"]',
    lookback_days int not null default 7,
    enabled boolean not null default true,
    created_by text not null default '',
    last_run_at timestamptz,
    last_status text,
    last_error text not null default '',
    next_run_at timestamptz not null default now(),
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table ai_tasks drop constraint if exists ai_tasks_lookback_check;
alter table ai_tasks add constraint ai_tasks_lookback_check
    check (lookback_days between 1 and 90);

alter table ai_tasks drop constraint if exists ai_tasks_last_status_check;
alter table ai_tasks add constraint ai_tasks_last_status_check
    check (last_status is null or last_status in ('succeeded', 'failed'));

create index if not exists idx_ai_tasks_due on ai_tasks (next_run_at) where enabled;

create table if not exists reports (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    ai_task_id bigint references ai_tasks(id) on delete set null,
    title text not null,
    content text not null default '',
    status text not null default 'succeeded',
    error text not null default '',
    trigger text not null default 'schedule',
    period_start timestamptz,
    period_end timestamptz,
    metrics jsonb,
    related_task_ids bigint[] not null default '{}',
    related_risk_ids bigint[] not null default '{}',
    created_by text not null default '',
    created_at timestamptz not null default now(),
    finished_at timestamptz
);

alter table reports drop constraint if exists reports_status_check;
alter table reports add constraint reports_status_check
    check (status in ('succeeded', 'failed'));

alter table reports drop constraint if exists reports_trigger_check;
alter table reports add constraint reports_trigger_check
    check (trigger in ('schedule', 'manual'));

create index if not exists idx_reports_group on reports (group_id, id desc);
create index if not exists idx_reports_ai_task on reports (ai_task_id, id desc);
