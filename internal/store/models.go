package store

import "time"

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Message struct {
	ID         int64      `json:"id"`
	GroupID    string     `json:"group_id"`
	SenderName string     `json:"sender_name"`
	Content    string     `json:"content"`
	Citations  []Citation `json:"citations,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Citation points an AI answer back to a retrieved document chunk.
type Citation struct {
	Index      int     `json:"index"`
	FileID     int64   `json:"file_id"`
	FileName   string  `json:"file_name"`
	ChunkIndex int     `json:"chunk_index"`
	Snippet    string  `json:"snippet"`
	Score      float64 `json:"score"`
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
	IndexStatus  string     `json:"index_status"`
	IndexError   string     `json:"index_error"`
	IndexedAt    *time.Time `json:"indexed_at,omitempty"`
	ChunkCount   int        `json:"chunk_count"`
	CreatedAt    time.Time  `json:"created_at"`
}

type FileContent struct {
	FileID    int64     `json:"file_id"`
	GroupID   string    `json:"group_id"`
	Content   string    `json:"content"`
	CharCount int       `json:"char_count"`
	CreatedAt time.Time `json:"created_at"`
}

type DocChunk struct {
	ID         int64     `json:"id"`
	FileID     int64     `json:"file_id"`
	GroupID    string    `json:"group_id"`
	ChunkIndex int       `json:"chunk_index"`
	Content    string    `json:"content"`
	CharCount  int       `json:"char_count"`
	CreatedAt  time.Time `json:"created_at"`
}

// RetrievedChunk is one vector-search hit together with its source file.
type RetrievedChunk struct {
	ChunkID    int64
	FileID     int64
	FileName   string
	ChunkIndex int
	Content    string
	Score      float64
}
