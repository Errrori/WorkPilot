// Package parser extracts Markdown from stored files through the Python sidecar
// and persists the result asynchronously.
package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const maxResponseBytes = 64 << 20

var (
	ErrUnsupported = errors.New("unsupported file type")
	ErrParseFailed = errors.New("parse failed")
)

// Client talks to the sidecar POST /parse endpoint.
type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
	}
}

// Parse streams the file to the sidecar and returns the extracted Markdown.
func (c *Client) Parse(ctx context.Context, fileName string, r io.Reader) (string, error) {
	pr, pw := io.Pipe()
	defer pr.Close()

	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", fileName)
		if err == nil {
			_, err = io.Copy(part, r)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/parse", pr)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read parse response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		detail := responseDetail(body)
		switch resp.StatusCode {
		case http.StatusUnsupportedMediaType:
			return "", fmt.Errorf("%w: %s", ErrUnsupported, detail)
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return "", fmt.Errorf("%w: %s", ErrParseFailed, detail)
		default:
			return "", fmt.Errorf("sidecar returned %d: %s", resp.StatusCode, detail)
		}
	}

	var parsed struct {
		FileName string `json:"filename"`
		Markdown string `json:"markdown"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("decode parse response: %w", err)
	}
	if strings.TrimSpace(parsed.Markdown) == "" {
		return "", fmt.Errorf("%w: no text extracted", ErrParseFailed)
	}
	return parsed.Markdown, nil
}

func responseDetail(body []byte) string {
	var payload struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Detail != "" {
		return payload.Detail
	}
	return strings.TrimSpace(string(body))
}
