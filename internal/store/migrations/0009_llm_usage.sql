create table if not exists llm_usage (
    id bigserial primary key,
    group_id uuid references groups(id) on delete cascade,
    source text not null,
    provider text not null default '',
    model text not null default '',
    prompt_tokens int not null default 0,
    completion_tokens int not null default 0,
    total_tokens int not null default 0,
    latency_ms int not null default 0,
    status text not null default 'succeeded',
    error text not null default '',
    created_at timestamptz not null default now()
);

alter table llm_usage drop constraint if exists llm_usage_status_check;
alter table llm_usage add constraint llm_usage_status_check
    check (status in ('succeeded', 'failed'));

create index if not exists idx_llm_usage_group_created on llm_usage (group_id, created_at desc);
create index if not exists idx_llm_usage_source on llm_usage (source, created_at desc);
