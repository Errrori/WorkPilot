create table if not exists files (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    uploader_name text not null,
    file_name text not null,
    content_type text not null default '',
    size_bytes bigint not null,
    storage_path text not null,
    created_at timestamptz not null default now()
);

create index if not exists idx_files_group_id_id on files (group_id, id);
