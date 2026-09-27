package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

const messageColumns = `id, group_id::text, sender_name, content, citations, created_at`

// SenderAI is the chat identity of AI-published messages (QA answers,
// generated reports).
const SenderAI = "WorkPilot AI"

func scanMessage(row interface {
	Scan(dest ...any) error
}) (Message, error) {
	var m Message
	var raw []byte
	err := row.Scan(&m.ID, &m.GroupID, &m.SenderName, &m.Content, &raw, &m.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.Citations); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func InsertMessage(ctx context.Context, pool *pgxpool.Pool, groupID, senderName, content string, citations []Citation) (Message, error) {
	raw, err := encodeCitations(citations)
	if err != nil {
		return Message{}, err
	}
	return scanMessage(pool.QueryRow(ctx,
		`insert into messages (group_id, sender_name, content, citations)
		 values ($1::uuid, $2, $3, $4)
		 returning `+messageColumns,
		groupID, senderName, content, raw,
	))
}

func ListMessages(ctx context.Context, pool *pgxpool.Pool, groupID string, limit int) ([]Message, error) {
	rows, err := pool.Query(ctx,
		`select `+messageColumns+`
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
		m, err := scanMessage(rows)
		if err != nil {
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

func encodeCitations(citations []Citation) ([]byte, error) {
	if len(citations) == 0 {
		return nil, nil
	}
	return json.Marshal(citations)
}

// QaStore adapts message persistence to the qa service.
type QaStore struct {
	Pool *pgxpool.Pool
}

func (s QaStore) InsertMessage(ctx context.Context, groupID, senderName, content string, citations []Citation) (Message, error) {
	return InsertMessage(ctx, s.Pool, groupID, senderName, content, citations)
}
