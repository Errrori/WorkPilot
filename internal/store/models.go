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

type Task struct {
	ID          int64      `json:"id"`
	GroupID     string     `json:"group_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Assignee    string     `json:"assignee"`
	Status      string     `json:"status"`
	Priority    string     `json:"priority"`
	Source      string     `json:"source"`
	Citations   []Citation `json:"citations,omitempty"`
	CreatedBy   string     `json:"created_by"`
	ConfirmedBy string     `json:"confirmed_by,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// AiTask is a custom scheduled AI task that produces reports.
type AiTask struct {
	ID           int64      `json:"id"`
	GroupID      string     `json:"group_id"`
	Name         string     `json:"name"`
	Prompt       string     `json:"prompt"`
	Schedule     string     `json:"schedule"`
	Timezone     string     `json:"timezone"`
	Sources      []string   `json:"sources"`
	LookbackDays int        `json:"lookback_days"`
	Enabled      bool       `json:"enabled"`
	CreatedBy    string     `json:"created_by"`
	LastRunAt    *time.Time `json:"last_run_at,omitempty"`
	LastStatus   string     `json:"last_status,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	NextRunAt    time.Time  `json:"next_run_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ReportMetrics is the deterministic snapshot of the report period.
type ReportMetrics struct {
	Messages     int            `json:"messages"`
	Files        int            `json:"files"`
	Tasks        map[string]int `json:"tasks"`
	Risks        map[string]int `json:"risks"`
	Repos        int            `json:"repos,omitempty"`
	PullRequests map[string]int `json:"pull_requests,omitempty"`
	Issues       map[string]int `json:"issues,omitempty"`
	Commits      int            `json:"commits,omitempty"`
}

// Repo is one bound GitHub repository whose activity feeds progress signals.
type Repo struct {
	ID           int64      `json:"id"`
	GroupID      string     `json:"group_id"`
	Provider     string     `json:"provider"`
	Owner        string     `json:"owner"`
	Name         string     `json:"name"`
	Enabled      bool       `json:"enabled"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	LastStatus   string     `json:"last_status,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	NextSyncAt   time.Time  `json:"next_sync_at"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// RepoItem is one synced pull request, issue or commit.
type RepoItem struct {
	ID              int64      `json:"id"`
	RepoID          int64      `json:"repo_id"`
	Kind            string     `json:"kind"`
	ExternalID      string     `json:"external_id"`
	Number          *int       `json:"number,omitempty"`
	Title           string     `json:"title"`
	State           string     `json:"state"`
	Author          string     `json:"author"`
	URL             string     `json:"url"`
	RemoteUpdatedAt *time.Time `json:"remote_updated_at,omitempty"`
	RepoOwner       string     `json:"repo_owner,omitempty"`
	RepoName        string     `json:"repo_name,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Report is one generated report (scheduled run or manual trigger).
type Report struct {
	ID             int64          `json:"id"`
	GroupID        string         `json:"group_id"`
	AiTaskID       *int64         `json:"ai_task_id,omitempty"`
	Title          string         `json:"title"`
	Content        string         `json:"content"`
	Status         string         `json:"status"`
	Error          string         `json:"error,omitempty"`
	Trigger        string         `json:"trigger"`
	PeriodStart    *time.Time     `json:"period_start,omitempty"`
	PeriodEnd      *time.Time     `json:"period_end,omitempty"`
	Metrics        *ReportMetrics `json:"metrics,omitempty"`
	RelatedTaskIDs []int64        `json:"related_task_ids,omitempty"`
	RelatedRiskIDs []int64        `json:"related_risk_ids,omitempty"`
	CreatedBy      string         `json:"created_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	FinishedAt     *time.Time     `json:"finished_at,omitempty"`
}

// Risk is a project risk raised manually or suggested by AI.
type Risk struct {
	ID             int64      `json:"id"`
	GroupID        string     `json:"group_id"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	Severity       string     `json:"severity"`
	Status         string     `json:"status"`
	Owner          string     `json:"owner"`
	Source         string     `json:"source"`
	Citations      []Citation `json:"citations,omitempty"`
	RelatedTaskIDs []int64    `json:"related_task_ids,omitempty"`
	CreatedBy      string     `json:"created_by"`
	ConfirmedBy    string     `json:"confirmed_by,omitempty"`
	ConfirmedAt    *time.Time `json:"confirmed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
