create extension if not exists vector;

create table if not exists groups (
    id uuid primary key default gen_random_uuid(),
    name text not null,
    created_at timestamptz not null default now()
);

create table if not exists messages (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    sender_name text not null,
    content text not null,
    created_at timestamptz not null default now()
);

create index if not exists idx_messages_group_id_id on messages (group_id, id desc);

insert into groups (id, name)
values ('00000000-0000-0000-0000-000000000001', '演示项目群')
on conflict (id) do nothing;
