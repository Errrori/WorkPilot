package docsync

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Errrori/workpilot/internal/gitsync"
	"github.com/Errrori/workpilot/internal/store"
	"github.com/Errrori/workpilot/internal/ws"
)

type fakeGitHub struct {
	info       gitsync.RepoInfo
	tree       gitsync.TreeResult
	treeErr    error
	blobs      map[string]string
	blobErr    map[string]error
	treeCalls  int
	blobCalls  int
	lastETag   string
	lastRef    string
	lastMaxLen int64
}

func (g *fakeGitHub) Repo(context.Context, string, string) (gitsync.RepoInfo, error) {
	return g.info, nil
}

func (g *fakeGitHub) Tree(_ context.Context, _, _, ref, etag string) (gitsync.TreeResult, error) {
	g.treeCalls++
	g.lastRef = ref
	g.lastETag = etag
	return g.tree, g.treeErr
}

func (g *fakeGitHub) Blob(_ context.Context, _, _, sha string, maxBytes int64) ([]byte, error) {
	g.blobCalls++
	g.lastMaxLen = maxBytes
	if err := g.blobErr[sha]; err != nil {
		return nil, err
	}
	return []byte(g.blobs[sha]), nil
}

type markCall struct {
	status   string
	errMsg   string
	syncedAt *time.Time
	nextSync time.Time
	etag     string
}

type fakeStore struct {
	source      store.DocSource
	items       map[string]store.DocSourceItem
	files       []store.File
	nextFileID  int64
	marks       []markCall
	archived    []string
	activated   []int64
	activateSrc store.DocSource
	activateOK  bool
}

func newFakeStore(source store.DocSource) *fakeStore {
	return &fakeStore{
		source:     source,
		items:      map[string]store.DocSourceItem{},
		nextFileID: 100,
	}
}

func (s *fakeStore) ListDueDocSources(context.Context, int) ([]store.DocSource, error) {
	return []store.DocSource{s.source}, nil
}

func (s *fakeStore) GetDocSourceByID(context.Context, int64) (store.DocSource, error) {
	return s.source, nil
}

func (s *fakeStore) ListDocSourceItemRecords(context.Context, int64) ([]store.DocSourceItem, error) {
	out := make([]store.DocSourceItem, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

func (s *fakeStore) UpsertDocSourceItem(_ context.Context, sourceID int64, in store.DocSourceItemInput) (store.DocSourceItem, error) {
	item := s.items[in.ExternalID]
	item.SourceID = sourceID
	item.ExternalID = in.ExternalID
	item.ContentHash = in.ContentHash
	item.RemoteUpdatedAt = in.RemoteUpdatedAt
	item.FileID = in.FileID
	item.PendingFileID = in.PendingFileID
	item.State = in.State
	item.Active = in.Active
	s.items[in.ExternalID] = item
	return item, nil
}

func (s *fakeStore) ArchiveDocSourceItem(_ context.Context, _ int64, externalID string) error {
	item := s.items[externalID]
	item.State = store.DocSourceItemExcluded
	item.Active = false
	item.FileID = nil
	item.PendingFileID = nil
	s.items[externalID] = item
	s.archived = append(s.archived, externalID)
	return nil
}

func (s *fakeStore) MarkDocSourceSync(_ context.Context, _ int64, status, errMessage string, _ time.Time, syncedAt *time.Time, nextSync time.Time, etag string) (store.DocSource, error) {
	s.marks = append(s.marks, markCall{status: status, errMsg: errMessage, syncedAt: syncedAt, nextSync: nextSync, etag: etag})
	s.source.LastStatus = status
	s.source.LastError = errMessage
	if syncedAt != nil {
		s.source.LastSyncedAt = syncedAt
	}
	s.source.NextSyncAt = nextSync
	s.source.LastETag = etag
	return s.source, nil
}

func (s *fakeStore) InsertSyncedFile(_ context.Context, in store.SyncedFileInsert) (store.File, error) {
	s.nextFileID++
	file := store.File{
		ID:           s.nextFileID,
		GroupID:      in.GroupID,
		UploaderName: in.UploaderName,
		FileName:     in.FileName,
		ContentType:  in.ContentType,
		StoragePath:  in.StoragePath,
		SizeBytes:    in.SizeBytes,
		Origin:       in.Origin,
		SourceID:     &in.SourceID,
		SourceURL:    in.SourceURL,
		ContentHash:  in.ContentHash,
		Active:       in.Active,
		ParseStatus:  store.ParseStatusPending,
		IndexStatus:  store.IndexStatusPending,
	}
	s.files = append(s.files, file)
	return file, nil
}

func (s *fakeStore) ActivateDocFile(_ context.Context, fileID int64) (store.DocSource, bool, error) {
	for path, item := range s.items {
		if item.PendingFileID != nil && *item.PendingFileID == fileID {
			item.FileID = &fileID
			item.PendingFileID = nil
			item.Active = true
			item.State = store.DocSourceItemIncluded
			s.items[path] = item
			s.activated = append(s.activated, fileID)
			return s.source, true, nil
		}
	}
	return store.DocSource{}, false, nil
}

type fakeFiles struct {
	saved   map[string]string
	removed []string
}

func (f *fakeFiles) Save(_ string, originalName string, r io.Reader, _ int64) (string, int64, error) {
	content, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	if f.saved == nil {
		f.saved = map[string]string{}
	}
	rel := "group/" + originalName
	f.saved[rel] = string(content)
	return rel, int64(len(content)), nil
}

func (f *fakeFiles) Remove(rel string) error {
	f.removed = append(f.removed, rel)
	return nil
}

type fakeParser struct {
	files []store.File
}

func (p *fakeParser) Enqueue(f store.File) {
	p.files = append(p.files, f)
}

type fakeNotifier struct {
	events  []string
	sources []store.DocSource
}

func (n *fakeNotifier) BroadcastDocSource(event string, s *store.DocSource) {
	n.events = append(n.events, event)
	if s != nil {
		n.sources = append(n.sources, *s)
	}
}

func testSource() store.DocSource {
	return store.DocSource{
		ID:         7,
		GroupID:    "group-1",
		Provider:   store.DocSourceProviderGitHub,
		Name:       "acme/docs",
		Config:     store.DocSourceConfig{Owner: "acme", Name: "docs", Ref: "main"},
		Enabled:    true,
		NextSyncAt: time.Now(),
	}
}

func testScheduler(t *testing.T, gh GitHub, st SyncStore, files FileWriter, parser ParseEnqueuer, notifier Notifier) *Scheduler {
	t.Helper()
	return &Scheduler{cfg: SchedulerConfig{
		Interval: 10 * time.Minute,
		Client:   gh,
		Store:    st,
		Files:    files,
		Parser:   parser,
		Notifier: notifier,
	}}
}

func TestSyncSourceDownloadsChangedFileAndWaitsForIndex(t *testing.T) {
	source := testSource()
	st := newFakeStore(source)
	gh := &fakeGitHub{
		tree: gitsync.TreeResult{
			ETag: `W/"t1"`,
			Entries: []gitsync.TreeEntry{
				{Path: "docs/PRD.md", Type: "blob", SHA: "sha1", Size: 10},
				{Path: "docs", Type: "tree", SHA: "dir"},
				{Path: "docs/main.go", Type: "blob", SHA: "sha-go", Size: 10},
			},
		},
		blobs: map[string]string{"sha1": "# PRD"},
	}
	files := &fakeFiles{}
	parser := &fakeParser{}
	notifier := &fakeNotifier{}
	sched := testScheduler(t, gh, st, files, parser, notifier)

	if err := sched.SyncSource(context.Background(), source); err != nil {
		t.Fatalf("SyncSource: %v", err)
	}
	if gh.blobCalls != 1 {
		t.Fatalf("blob calls = %d", gh.blobCalls)
	}
	if len(st.files) != 1 || st.files[0].Active || st.files[0].Origin != store.FileOriginGitHub {
		t.Fatalf("files = %#v", st.files)
	}
	item := st.items["docs/PRD.md"]
	if item.PendingFileID == nil || *item.PendingFileID != st.files[0].ID || item.FileID != nil {
		t.Fatalf("item = %#v", item)
	}
	if len(parser.files) != 1 || parser.files[0].ID != st.files[0].ID {
		t.Fatalf("parser files = %#v", parser.files)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.DocSourceStatusSucceeded || st.marks[0].syncedAt == nil {
		t.Fatalf("marks = %#v", st.marks)
	}
	if st.marks[0].etag != `W/"t1"` {
		t.Fatalf("etag = %q", st.marks[0].etag)
	}
	if len(notifier.events) != 1 || notifier.events[0] != ws.EventDocSourceSynced {
		t.Fatalf("events = %#v", notifier.events)
	}

	sched.OnFileIndexed(context.Background(), st.files[0].ID)
	item = st.items["docs/PRD.md"]
	if item.PendingFileID != nil || item.FileID == nil || *item.FileID != st.files[0].ID {
		t.Fatalf("activated item = %#v", item)
	}
	if len(notifier.events) != 2 || notifier.events[1] != ws.EventDocSourceUpdated {
		t.Fatalf("events = %#v", notifier.events)
	}
}

func TestSyncSourceSkipsUnchangedAndUsesETag(t *testing.T) {
	source := testSource()
	source.LastETag = `W/"old"`
	fileID := int64(5)
	st := newFakeStore(source)
	st.items["docs/PRD.md"] = store.DocSourceItem{
		ExternalID:  "docs/PRD.md",
		ContentHash: "same",
		FileID:      &fileID,
		State:       store.DocSourceItemIncluded,
		Active:      true,
	}
	gh := &fakeGitHub{tree: gitsync.TreeResult{
		ETag:    `W/"new"`,
		Entries: []gitsync.TreeEntry{{Path: "docs/PRD.md", Type: "blob", SHA: "same", Size: 10}},
	}}
	sched := testScheduler(t, gh, st, &fakeFiles{}, &fakeParser{}, &fakeNotifier{})

	if err := sched.SyncSource(context.Background(), source); err != nil {
		t.Fatalf("SyncSource: %v", err)
	}
	if gh.lastETag != `W/"old"` || gh.blobCalls != 0 {
		t.Fatalf("etag = %q blob calls = %d", gh.lastETag, gh.blobCalls)
	}
	if len(st.marks) != 1 || st.marks[0].syncedAt != nil {
		t.Fatalf("marks = %#v", st.marks)
	}

	gh.tree = gitsync.TreeResult{ETag: `W/"old"`, NotModified: true}
	if err := sched.SyncSource(context.Background(), source); err != nil {
		t.Fatalf("SyncSource not modified: %v", err)
	}
	if len(st.marks) != 2 || st.marks[1].syncedAt != nil || st.marks[1].etag != `W/"old"` {
		t.Fatalf("marks = %#v", st.marks)
	}
}

func TestSyncSourceArchivesRemovedAndOversizedPaths(t *testing.T) {
	source := testSource()
	source.Config.MaxFileMB = 1
	st := newFakeStore(source)
	fileID := int64(5)
	st.items["docs/gone.md"] = store.DocSourceItem{
		ExternalID: "docs/gone.md", ContentHash: "old", FileID: &fileID,
		State: store.DocSourceItemIncluded, Active: true,
	}
	st.items["docs/big.md"] = store.DocSourceItem{
		ExternalID: "docs/big.md", ContentHash: "old", FileID: &fileID,
		State: store.DocSourceItemIncluded, Active: true,
	}
	gh := &fakeGitHub{tree: gitsync.TreeResult{Entries: []gitsync.TreeEntry{
		{Path: "docs/big.md", Type: "blob", SHA: "big", Size: 2 << 20},
	}}}
	sched := testScheduler(t, gh, st, &fakeFiles{}, &fakeParser{}, &fakeNotifier{})

	if err := sched.SyncSource(context.Background(), source); err != nil {
		t.Fatalf("SyncSource: %v", err)
	}
	if len(st.archived) != 2 {
		t.Fatalf("archived = %#v", st.archived)
	}
	if gh.blobCalls != 0 {
		t.Fatalf("oversized file should not be downloaded, calls = %d", gh.blobCalls)
	}
	if st.marks[0].syncedAt == nil {
		t.Fatal("archiving should mark content changed")
	}
}

func TestSyncSourceMarksFailure(t *testing.T) {
	source := testSource()
	st := newFakeStore(source)
	gh := &fakeGitHub{treeErr: errors.New("boom")}
	sched := testScheduler(t, gh, st, &fakeFiles{}, &fakeParser{}, &fakeNotifier{})

	err := sched.SyncSource(context.Background(), source)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if len(st.marks) != 1 || st.marks[0].status != store.DocSourceStatusFailed || !strings.Contains(st.marks[0].errMsg, "boom") {
		t.Fatalf("marks = %#v", st.marks)
	}
}

func TestSyncSourceRecordsBlobFailureAndContinues(t *testing.T) {
	source := testSource()
	st := newFakeStore(source)
	gh := &fakeGitHub{
		tree: gitsync.TreeResult{Entries: []gitsync.TreeEntry{
			{Path: "docs/a.md", Type: "blob", SHA: "bad", Size: 10},
			{Path: "docs/b.md", Type: "blob", SHA: "ok", Size: 10},
		}},
		blobs:   map[string]string{"ok": "ok"},
		blobErr: map[string]error{"bad": errors.New("blob down")},
	}
	parser := &fakeParser{}
	sched := testScheduler(t, gh, st, &fakeFiles{}, parser, &fakeNotifier{})

	if err := sched.SyncSource(context.Background(), source); err != nil {
		t.Fatalf("SyncSource: %v", err)
	}
	if st.items["docs/a.md"].State != store.DocSourceItemFailed {
		t.Fatalf("item a = %#v", st.items["docs/a.md"])
	}
	if st.items["docs/b.md"].PendingFileID == nil {
		t.Fatalf("item b = %#v", st.items["docs/b.md"])
	}
	if len(st.marks) != 1 || st.marks[0].status != store.DocSourceStatusFailed || !strings.Contains(st.marks[0].errMsg, "blob down") {
		t.Fatalf("marks = %#v", st.marks)
	}
}
