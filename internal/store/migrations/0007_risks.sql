create table if not exists risks (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    title text not null,
    description text not null default '',
    severity text not null default 'medium',
    status text not null default 'suggested',
    owner text not null default '',
    source text not null default 'manual',
    citations jsonb,
    related_task_ids bigint[] not null default '{}',
    created_by text not null default '',
    confirmed_by text not null default '',
    confirmed_at timestamptz,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table risks drop constraint if exists risks_severity_check;
alter table risks add constraint risks_severity_check
    check (severity in ('low', 'medium', 'high'));

alter table risks drop constraint if exists risks_status_check;
alter table risks add constraint risks_status_check
    check (status in ('suggested', 'open', 'mitigating', 'resolved', 'dismissed'));

alter table risks drop constraint if exists risks_source_check;
alter table risks add constraint risks_source_check
    check (source in ('manual', 'extracted'));

create index if not exists idx_risks_group_id_status on risks (group_id, status, updated_at desc);
