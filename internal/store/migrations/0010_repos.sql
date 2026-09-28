create table if not exists repos (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    provider text not null default 'github',
    owner text not null,
    name text not null,
    enabled boolean not null default true,
    last_synced_at timestamptz,
    last_status text,
    last_error text not null default '',
    next_sync_at timestamptz not null default now(),
    created_by text not null default '',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table repos drop constraint if exists repos_provider_check;
alter table repos add constraint repos_provider_check
    check (provider in ('github'));

alter table repos drop constraint if exists repos_last_status_check;
alter table repos add constraint repos_last_status_check
    check (last_status is null or last_status in ('succeeded', 'failed'));

create unique index if not exists idx_repos_group_ref on repos (group_id, provider, owner, name);
create index if not exists idx_repos_due on repos (next_sync_at) where enabled;

create table if not exists repo_items (
    id bigserial primary key,
    repo_id bigint not null references repos(id) on delete cascade,
    kind text not null,
    external_id text not null,
    number int,
    title text not null default '',
    state text not null default '',
    author text not null default '',
    url text not null default '',
    remote_updated_at timestamptz,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table repo_items drop constraint if exists repo_items_kind_check;
alter table repo_items add constraint repo_items_kind_check
    check (kind in ('pull_request', 'issue', 'commit'));

alter table repo_items drop constraint if exists repo_items_state_check;
alter table repo_items add constraint repo_items_state_check
    check (state in ('', 'open', 'closed', 'merged'));

create unique index if not exists idx_repo_items_external on repo_items (repo_id, kind, external_id);
create index if not exists idx_repo_items_recent on repo_items (repo_id, remote_updated_at desc);

update ai_tasks set sources = sources || '["git"]'::jsonb where not (sources ? 'git');
