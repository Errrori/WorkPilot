alter table files
    add column if not exists index_status text not null default 'pending',
    add column if not exists index_error text not null default '',
    add column if not exists indexed_at timestamptz,
    add column if not exists chunk_count integer not null default 0;

alter table files drop constraint if exists files_index_status_check;
alter table files add constraint files_index_status_check
    check (index_status in ('pending', 'indexing', 'indexed', 'failed', 'skipped'));

update files set index_status = 'skipped'
where parse_status in ('failed', 'unsupported');

create table if not exists doc_chunks (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    file_id bigint not null references files(id) on delete cascade,
    chunk_index integer not null,
    content text not null,
    char_count integer not null,
    embedding vector(1024) not null,
    created_at timestamptz not null default now(),
    unique (file_id, chunk_index)
);

create index if not exists idx_doc_chunks_group_id on doc_chunks (group_id);
create index if not exists idx_doc_chunks_embedding on doc_chunks using hnsw (embedding vector_cosine_ops);
