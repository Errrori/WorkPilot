package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func InsertMessage(ctx context.Context, pool *pgxpool.Pool, groupID, senderName, content string) (Message, error) {
	var m Message
	err := pool.QueryRow(ctx,
		`insert into messages (group_id, sender_name, content)
		 values ($1::uuid, $2, $3)
		 returning id, group_id::text, sender_name, content, created_at`,
		groupID, senderName, content,
	).Scan(&m.ID, &m.GroupID, &m.SenderName, &m.Content, &m.CreatedAt)
	return m, err
}

func ListMessages(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]Message, error) {
	rows, err := pool.Query(ctx,
		`select id, group_id::text, sender_name, content, created_at
		 from messages
		 where group_id = $1::uuid
		 order by id desc
		 limit $2`,
		groupID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.GroupID, &m.SenderName, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}
