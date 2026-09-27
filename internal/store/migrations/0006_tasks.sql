create table if not exists tasks (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    title text not null,
    description text not null default '',
    assignee text not null default '',
    status text not null default 'suggested',
    priority text not null default 'medium',
    source text not null default 'manual',
    citations jsonb,
    created_by text not null default '',
    confirmed_by text not null default '',
    confirmed_at timestamptz,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table tasks drop constraint if exists tasks_status_check;
alter table tasks add constraint tasks_status_check
    check (status in ('suggested', 'todo', 'doing', 'done', 'rejected'));

alter table tasks drop constraint if exists tasks_priority_check;
alter table tasks add constraint tasks_priority_check
    check (priority in ('low', 'medium', 'high'));

alter table tasks drop constraint if exists tasks_source_check;
alter table tasks add constraint tasks_source_check
    check (source in ('manual', 'extracted'));

create index if not exists idx_tasks_group_id_status on tasks (group_id, status, updated_at desc);
