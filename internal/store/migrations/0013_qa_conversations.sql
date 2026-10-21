create table if not exists qa_conversations (
    id bigserial primary key,
    group_id uuid not null references groups(id) on delete cascade,
    user_name text not null,
    title text not null default '',
    created_at timestamptz not null default now(),
    updated_at timestamptz not null default now()
);

create index if not exists idx_qa_conversations_owner
    on qa_conversations (group_id, user_name, updated_at desc, id desc);

create table if not exists qa_messages (
    id bigserial primary key,
    conversation_id bigint not null references qa_conversations(id) on delete cascade,
    role text not null,
    content text not null,
    citations jsonb,
    created_at timestamptz not null default now()
);

alter table qa_messages drop constraint if exists qa_messages_role_check;
alter table qa_messages add constraint qa_messages_role_check
    check (role in ('user', 'assistant'));

create index if not exists idx_qa_messages_conversation
    on qa_messages (conversation_id, id);
