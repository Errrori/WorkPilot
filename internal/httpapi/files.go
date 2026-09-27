package httpapi

import (
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Errrori/workpilot/internal/storage"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

const (
	multipartOverheadBytes = 1 << 20
	multipartMemoryBytes   = 32 << 20
	maxFileNameRunes       = 200
)

// ParseEnqueuer schedules stored files for asynchronous parsing.
type ParseEnqueuer interface {
	Enqueue(f store.File)
}

// IndexEnqueuer schedules parsed files for asynchronous chunk indexing.
type IndexEnqueuer interface {
	Enqueue(f store.File)
}

func uploadFile(pool *pgxpool.Pool, files *storage.Store, hub *ws.Hub, enqueuer ParseEnqueuer, maxUploadBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}

		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes+multipartOverheadBytes)
		if err := c.Request.ParseMultipartForm(multipartMemoryBytes); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				c.JSON(http.StatusRequestEntityTooLarge, tooLargeBody(maxUploadBytes))
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid multipart form"})
			return
		}

		uploader := strings.TrimSpace(c.Query("user"))
		if uploader == "" {
			uploader = strings.TrimSpace(c.Request.FormValue("user"))
		}
		if uploader == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "user is required"})
			return
		}

		_, header, err := c.Request.FormFile("file")
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "file field is required"})
			return
		}

		fileName := sanitizeFileName(header.Filename)
		if fileName == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "file name is required"})
			return
		}

		src, err := header.Open()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read uploaded file"})
			return
		}
		defer src.Close()

		relPath, size, err := files.Save(groupID, fileName, src, maxUploadBytes)
		if errors.Is(err, storage.ErrTooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, tooLargeBody(maxUploadBytes))
			return
		}
		if err != nil {
			log.Printf("save uploaded file: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store file"})
			return
		}

		contentType := header.Header.Get("Content-Type")
		if contentType == "" {
			contentType = mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
		}

		record, err := store.InsertFile(c.Request.Context(), pool, groupID, uploader, fileName, contentType, relPath, size)
		if err != nil {
			if rmErr := files.Remove(relPath); rmErr != nil {
				log.Printf("remove orphan file %s: %v", relPath, rmErr)
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		hub.BroadcastFile(ws.EventFileUploaded, &record)
		enqueuer.Enqueue(record)
		c.JSON(http.StatusCreated, gin.H{"file": record})
	}
}

func listFiles(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !ensureGroup(c, pool, c.Param("id")) {
			return
		}
		files, err := store.ListFiles(c.Request.Context(), pool, c.Param("id"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if files == nil {
			files = []store.File{}
		}
		c.JSON(http.StatusOK, gin.H{"files": files})
	}
}

func getFileContent(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		if !ensureGroup(c, pool, c.Param("id")) {
			return
		}
		content, err := store.GetFileContent(c.Request.Context(), pool, c.Param("id"), fileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "parsed content not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, content)
	}
}

func retryFileParse(pool *pgxpool.Pool, enqueuer ParseEnqueuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		record, err := store.GetFile(c.Request.Context(), pool, groupID, fileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if record.ParseStatus == store.ParseStatusParsing {
			c.JSON(http.StatusConflict, gin.H{"error": "file is already being parsed"})
			return
		}
		if record.ParseStatus != store.ParseStatusPending {
			record, err = store.MarkFileParsePending(c.Request.Context(), pool, groupID, fileID)
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusConflict, gin.H{"error": "file is already being parsed"})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		enqueuer.Enqueue(record)
		c.JSON(http.StatusAccepted, gin.H{"file": record})
	}
}

func reindexFile(pool *pgxpool.Pool, enqueuer IndexEnqueuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		record, err := store.GetFile(c.Request.Context(), pool, groupID, fileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if record.ParseStatus != store.ParseStatusParsed {
			c.JSON(http.StatusConflict, gin.H{"error": "file is not parsed yet"})
			return
		}
		if record.IndexStatus == store.IndexStatusIndexing {
			c.JSON(http.StatusConflict, gin.H{"error": "file is already being indexed"})
			return
		}
		if record.IndexStatus != store.IndexStatusPending {
			record, err = store.MarkFileIndexPending(c.Request.Context(), pool, groupID, fileID)
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusConflict, gin.H{"error": "file is already being indexed"})
				return
			}
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
		enqueuer.Enqueue(record)
		c.JSON(http.StatusAccepted, gin.H{"file": record})
	}
}

func listFileChunks(pool *pgxpool.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		groupID := c.Param("id")
		if !ensureGroup(c, pool, groupID) {
			return
		}
		chunks, err := store.ListFileChunks(c.Request.Context(), pool, groupID, fileID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if len(chunks) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "chunks not found (file missing or not indexed)"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"file_id": fileID, "chunk_count": len(chunks), "chunks": chunks})
	}
}

func downloadFile(pool *pgxpool.Pool, files *storage.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		record, err := store.GetFile(c.Request.Context(), pool, c.Param("id"), fileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		path, err := files.Path(record.StoragePath)
		if err != nil {
			log.Printf("resolve storage path for file %d: %v", record.ID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid storage path"})
			return
		}
		c.Header("Content-Disposition", contentDisposition(record.FileName))
		if record.ContentType != "" {
			c.Header("Content-Type", record.ContentType)
		}
		c.File(path)
	}
}

func deleteFile(pool *pgxpool.Pool, files *storage.Store, hub *ws.Hub) gin.HandlerFunc {
	return func(c *gin.Context) {
		fileID, ok := parseFileID(c)
		if !ok {
			return
		}
		record, err := store.DeleteFile(c.Request.Context(), pool, c.Param("id"), fileID)
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "file not found"})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := files.Remove(record.StoragePath); err != nil {
			log.Printf("remove stored file %s: %v", record.StoragePath, err)
		}
		hub.BroadcastFile(ws.EventFileDeleted, &record)
		c.JSON(http.StatusOK, gin.H{"deleted": true, "file": record})
	}
}

func ensureGroup(c *gin.Context, pool *pgxpool.Pool, groupID string) bool {
	exists, err := store.GroupExists(c.Request.Context(), pool, groupID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return false
	}
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
		return false
	}
	return true
}

func parseFileID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("fileID"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid file id"})
		return 0, false
	}
	return id, true
}

func tooLargeBody(maxUploadBytes int64) gin.H {
	return gin.H{"error": fmt.Sprintf("file exceeds %d MB limit", maxUploadBytes>>20)}
}

func sanitizeFileName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	runes := []rune(name)
	if len(runes) > maxFileNameRunes {
		name = string(runes[:maxFileNameRunes])
	}
	return name
}

func contentDisposition(fileName string) string {
	fallback := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';' {
			return '_'
		}
		return r
	}, fileName)
	if strings.TrimSpace(fallback) == "" {
		fallback = "file"
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(fileName))
}
