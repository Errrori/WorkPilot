alter table files
    add column if not exists parse_status text not null default 'pending',
    add column if not exists parse_error text not null default '',
    add column if not exists parsed_at timestamptz;

alter table files drop constraint if exists files_parse_status_check;
alter table files add constraint files_parse_status_check
    check (parse_status in ('pending', 'parsing', 'parsed', 'failed', 'unsupported'));

create table if not exists file_contents (
    file_id bigint primary key references files(id) on delete cascade,
    group_id uuid not null references groups(id) on delete cascade,
    content text not null,
    char_count integer not null,
    created_at timestamptz not null default now()
);

create index if not exists idx_file_contents_group_id on file_contents (group_id);
