package store

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

const qaConversationColumns = `id, group_id::text, user_name, title, created_at, updated_at`
const qaMessageColumns = `id, conversation_id, role, content, rewritten_query, citations, created_at`

const (
	QARoleUser      = "user"
	QARoleAssistant = "assistant"
)

func scanQAConversation(row interface {
	Scan(dest ...any) error
}) (QAConversation, error) {
	var c QAConversation
	err := row.Scan(&c.ID, &c.GroupID, &c.UserName, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func scanQAMessage(row interface {
	Scan(dest ...any) error
}) (QAMessage, error) {
	var m QAMessage
	var raw []byte
	err := row.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.RewrittenQuery, &raw, &m.CreatedAt)
	if err != nil {
		return QAMessage{}, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m.Citations); err != nil {
			return QAMessage{}, err
		}
	}
	return m, nil
}

func CreateQAConversation(ctx context.Context, pool *pgxpool.Pool, groupID, userName, title string) (QAConversation, error) {
	return scanQAConversation(pool.QueryRow(ctx,
		`insert into qa_conversations (group_id, user_name, title)
		 values ($1::uuid, $2, $3)
		 returning `+qaConversationColumns,
		groupID, userName, title,
	))
}

func GetQAConversation(ctx context.Context, pool *pgxpool.Pool, groupID, userName string, id int64) (QAConversation, error) {
	return scanQAConversation(pool.QueryRow(ctx,
		`select `+qaConversationColumns+`
		 from qa_conversations
		 where id = $1 and group_id = $2::uuid and user_name = $3`,
		id, groupID, userName,
	))
}

func ListQAConversations(ctx context.Context, pool *pgxpool.Pool, groupID, userName string, limit int) ([]QAConversation, error) {
	rows, err := pool.Query(ctx,
		`select `+qaConversationColumns+`
		 from qa_conversations
		 where group_id = $1::uuid and user_name = $2
		 order by updated_at desc, id desc
		 limit $3`,
		groupID, userName, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var conversations []QAConversation
	for rows.Next() {
		c, err := scanQAConversation(rows)
		if err != nil {
			return nil, err
		}
		conversations = append(conversations, c)
	}
	return conversations, rows.Err()
}

// AppendQAMessage stores one turn and bumps the conversation's updated_at.
// rewrittenQuery is set only for user turns that were rewritten for retrieval.
func AppendQAMessage(ctx context.Context, pool *pgxpool.Pool, conversationID int64, role, content string, citations []Citation, rewrittenQuery *string) (QAMessage, error) {
	raw, err := encodeCitations(citations)
	if err != nil {
		return QAMessage{}, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return QAMessage{}, err
	}
	defer tx.Rollback(ctx)

	msg, err := scanQAMessage(tx.QueryRow(ctx,
		`insert into qa_messages (conversation_id, role, content, rewritten_query, citations)
		 values ($1, $2, $3, $4, $5)
		 returning `+qaMessageColumns,
		conversationID, role, content, rewrittenQuery, raw,
	))
	if err != nil {
		return QAMessage{}, err
	}
	if _, err := tx.Exec(ctx, `update qa_conversations set updated_at = now() where id = $1`, conversationID); err != nil {
		return QAMessage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return QAMessage{}, err
	}
	return msg, nil
}

func ListQAMessages(ctx context.Context, pool *pgxpool.Pool, conversationID int64, limit int) ([]QAMessage, error) {
	rows, err := pool.Query(ctx,
		`select `+qaMessageColumns+`
		 from qa_messages
		 where conversation_id = $1
		 order by id desc
		 limit $2`,
		conversationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []QAMessage
	for rows.Next() {
		m, err := scanQAMessage(rows)
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

// QAStore adapts QA conversation persistence to the qa service.
type QAStore struct {
	Pool *pgxpool.Pool
}

func (s QAStore) CreateConversation(ctx context.Context, groupID, userName, title string) (QAConversation, error) {
	return CreateQAConversation(ctx, s.Pool, groupID, userName, title)
}

func (s QAStore) GetConversation(ctx context.Context, groupID, userName string, id int64) (QAConversation, error) {
	return GetQAConversation(ctx, s.Pool, groupID, userName, id)
}

func (s QAStore) AppendMessage(ctx context.Context, conversationID int64, role, content string, citations []Citation, rewrittenQuery *string) (QAMessage, error) {
	return AppendQAMessage(ctx, s.Pool, conversationID, role, content, citations, rewrittenQuery)
}

func (s QAStore) ListMessages(ctx context.Context, conversationID int64, limit int) ([]QAMessage, error) {
	return ListQAMessages(ctx, s.Pool, conversationID, limit)
}
