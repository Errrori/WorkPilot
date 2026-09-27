package store

import "time"

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Message struct {
	ID         int64     `json:"id"`
	GroupID    string    `json:"group_id"`
	SenderName string    `json:"sender_name"`
	Content    string    `json:"content"`
	CreatedAt  time.Time `json:"created_at"`
}

type File struct {
	ID           int64      `json:"id"`
	GroupID      string     `json:"group_id"`
	UploaderName string     `json:"uploader_name"`
	FileName     string     `json:"file_name"`
	ContentType  string     `json:"content_type"`
	SizeBytes    int64      `json:"size_bytes"`
	StoragePath  string     `json:"-"`
	ParseStatus  string     `json:"parse_status"`
	ParseError   string     `json:"parse_error"`
	ParsedAt     *time.Time `json:"parsed_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type FileContent struct {
	FileID    int64     `json:"file_id"`
	GroupID   string    `json:"group_id"`
	Content   string    `json:"content"`
	CharCount int       `json:"char_count"`
	CreatedAt time.Time `json:"created_at"`
}
