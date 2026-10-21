alter table messages add column if not exists reply_to bigint;

alter table tasks add column if not exists origin text not null default 'manual';
alter table tasks add column if not exists source_message_ids bigint[];

update tasks set origin = 'document' where source = 'extracted' and origin = 'manual';

alter table tasks drop constraint if exists tasks_origin_check;
alter table tasks add constraint tasks_origin_check
    check (origin in ('manual', 'document', 'chat'));

create table if not exists agent_runs (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    trigger_message_id bigint not null,
    status text not null default 'running',
    trigger_text text not null default '',
    reply text not null default '',
    steps jsonb,
    tokens int not null default 0,
    latency_ms int not null default 0,
    error text not null default '',
    created_by text not null default '',
    created_at timestamptz not null default now(),
    finished_at timestamptz
);

alter table agent_runs drop constraint if exists agent_runs_status_check;
alter table agent_runs add constraint agent_runs_status_check
    check (status in ('running', 'succeeded', 'failed'));

create unique index if not exists idx_agent_runs_trigger on agent_runs (trigger_message_id);
create index if not exists idx_agent_runs_group on agent_runs (group_id, id desc);
