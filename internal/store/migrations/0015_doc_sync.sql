create table if not exists doc_sources (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    provider text not null default 'github',
    name text not null,
    config jsonb not null default '{}'::jsonb,
    enabled boolean not null default true,
    last_checked_at timestamptz,
    last_synced_at timestamptz,
    last_status text,
    last_error text not null default '',
    last_etag text not null default '',
    next_sync_at timestamptz not null default now(),
    created_by text not null default '',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table doc_sources drop constraint if exists doc_sources_provider_check;
alter table doc_sources add constraint doc_sources_provider_check
    check (provider in ('github'));

alter table doc_sources drop constraint if exists doc_sources_last_status_check;
alter table doc_sources add constraint doc_sources_last_status_check
    check (last_status is null or last_status in ('succeeded', 'failed'));

create unique index if not exists idx_doc_sources_group_name on doc_sources (group_id, provider, name);
create index if not exists idx_doc_sources_due on doc_sources (next_sync_at) where enabled;

alter table files
    add column if not exists origin text not null default 'upload',
    add column if not exists source_id bigint references doc_sources(id) on delete set null,
    add column if not exists source_url text not null default '',
    add column if not exists content_hash text not null default '',
    add column if not exists active boolean not null default true;

create index if not exists idx_files_group_active on files (group_id, active);
create index if not exists idx_files_source on files (source_id);

create table if not exists doc_source_items (
    id bigserial primary key,
    source_id bigint not null references doc_sources(id) on delete cascade,
    external_id text not null,
    content_hash text not null default '',
    remote_updated_at timestamptz,
    file_id bigint references files(id) on delete set null,
    pending_file_id bigint references files(id) on delete set null,
    state text not null default 'included',
    active boolean not null default true,
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

alter table doc_source_items drop constraint if exists doc_source_items_state_check;
alter table doc_source_items add constraint doc_source_items_state_check
    check (state in ('included', 'excluded', 'failed'));

create unique index if not exists idx_doc_source_items_external on doc_source_items (source_id, external_id);
create index if not exists idx_doc_source_items_active on doc_source_items (source_id, active);
create index if not exists idx_doc_source_items_pending on doc_source_items (pending_file_id) where pending_file_id is not null;
