alter table messages
    add column if not exists citations jsonb;
