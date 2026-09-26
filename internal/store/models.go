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
