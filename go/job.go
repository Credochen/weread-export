package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 事件 ----------

type Event struct {
	Type string `json:"type"` // meta | log | progress | done | error
	// 注意：Done/Total 不能 omitempty，否则第一帧 done=0 会被整字段吃掉，
	// 前端拿到 undefined 算出 NaN 进度。
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Title   string `json:"title,omitempty"`
	Message string `json:"message,omitempty"`
	File    string `json:"file,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Book    string `json:"book,omitempty"`
}

// ---------- 任务 ----------

type Job struct {
	ID       string
	BookName string
	FileName string

	mu     sync.Mutex
	events []Event
	subs   map[chan Event]struct{}
	status string
	errMsg string
	cancel context.CancelFunc
}

func newJob(id string) *Job {
	return &Job{ID: id, subs: map[chan Event]struct{}{}, status: "running"}
}

func (j *Job) emit(e Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, e)
	if e.Type == "done" {
		j.status = "done"
	}
	if e.Type == "error" {
		j.status = "error"
		j.errMsg = e.Message
	}
	for ch := range j.subs {
		select {
		case ch <- e:
		default: // 订阅者满了就丢，避免阻塞抓取
		}
	}
}

func (j *Job) subscribe() (chan Event, []Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	ch := make(chan Event, 128)
	j.subs[ch] = struct{}{}
	snapshot := make([]Event, len(j.events))
	copy(snapshot, j.events)
	return ch, snapshot
}

func (j *Job) unsubscribe(ch chan Event) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.subs[ch]; ok {
		delete(j.subs, ch)
		close(ch)
	}
}

func (j *Job) snapshot() Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	last := Event{Type: "meta", Message: j.status}
	for _, e := range j.events {
		if e.Type == "progress" {
			last.Done, last.Total, last.Title = e.Done, e.Total, e.Title
		}
		if e.Type == "done" || e.Type == "error" {
			last = e
		}
	}
	last.Book = j.BookName
	last.File = j.FileName
	if j.status == "running" {
		last.Type = "meta"
		last.Message = "running"
	}
	return last
}

type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  int
}

func NewManager() *Manager { return &Manager{jobs: map[string]*Job{}} }

func (m *Manager) add(j *Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.jobs[j.ID] = j
}

func (m *Manager) get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// ---------- 导出请求 ----------

type ExportRequest struct {
	Cookie    string `json:"cookie"`
	ReaderURL string `json:"readerUrl"`
	BookID    string `json:"bookId"`
	Mode      string `json:"mode"`  // all | limit | custom
	Limit     int    `json:"limit"` // Mode=limit 时生效
	Spec      string `json:"spec"`  // Mode=custom，如 "1-20,35,50-"
}

var unsafeName = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)
var repeatUnderscore = regexp.MustCompile(`_+`)

func safeFileName(title string) string {
	name := unsafeName.ReplaceAllString(strings.TrimSpace(title), "_")
	name = repeatUnderscore.ReplaceAllString(name, "_")
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "weread-book"
	}
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	return name
}

// selectChapters 按模式挑章节。Idx 从 1 开始。
func selectChapters(list []Chapter, req ExportRequest) ([]Chapter, error) {
	switch req.Mode {
	case "", "all":
		return list, nil
	case "limit":
		if req.Limit <= 0 || req.Limit > len(list) {
			req.Limit = len(list)
		}
		return list[:req.Limit], nil
	case "custom":
		want := map[int]bool{}
		for _, token := range strings.Split(req.Spec, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			lo, hi := 0, 0
			if strings.Contains(token, "-") {
				parts := strings.SplitN(token, "-", 2)
				l, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
				if err1 != nil {
					return nil, fmt.Errorf("范围写法看不懂：%q", token)
				}
				lo = l
				if strings.TrimSpace(parts[1]) == "" {
					hi = len(list)
				} else {
					h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
					if err2 != nil {
						return nil, fmt.Errorf("范围写法看不懂：%q", token)
					}
					hi = h
				}
			} else {
				l, err := strconv.Atoi(token)
				if err != nil {
					return nil, fmt.Errorf("范围写法看不懂：%q", token)
				}
				lo, hi = l, l
			}
			if lo > hi {
				lo, hi = hi, lo
			}
			for i := lo; i <= hi; i++ {
				want[i] = true
			}
		}
		var out []Chapter
		for _, ch := range list {
			if want[ch.Idx] {
				out = append(out, ch)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("所选范围内没有章节")
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Idx < out[j].Idx })
		return out, nil
	}
	return nil, fmt.Errorf("未知模式 %q", req.Mode)
}

// Start 创建任务并后台执行
func (m *Manager) Start(req ExportRequest, outDir string) (*Job, error) {
	job := newJob(fmt.Sprintf("j%d", time.Now().UnixNano()))
	ctx, cancel := context.WithCancel(context.Background())
	job.cancel = cancel
	m.add(job)

	go func() {
		defer cancel()
		err := runExport(ctx, job, req, outDir)
		if err != nil {
			job.emit(Event{Type: "error", Message: err.Error()})
		}
	}()
	return job, nil
}

func runExport(ctx context.Context, job *Job, req ExportRequest, outDir string) error {
	client := NewClient(req.Cookie)
	job.emit(Event{Type: "log", Message: "续期登录态…"})
	if !client.Renew() {
		job.emit(Event{Type: "log", Message: "续期未成功，继续尝试（Cookie 可能已过期）"})
	}

	readerURL := req.ReaderURL
	if readerURL == "" {
		if req.BookID == "" {
			return fmt.Errorf("缺少阅读页链接或 bookId")
		}
		readerURL = readerURLFor(req.BookID, "")
	}

	state, err := client.GetReaderState(readerURL)
	if err != nil {
		return err
	}
	job.BookName = state.Title
	job.emit(Event{Type: "log", Message: fmt.Sprintf("已识别《%s》%s", state.Title, state.Author)})

	cat, err := client.GetCatalog(state.BookID, readerURL)
	if err != nil {
		return err
	}
	if cat.Title != "" {
		job.BookName = cat.Title
	}
	job.emit(Event{Type: "log", Message: fmt.Sprintf("目录 %d 章，格式 %s", len(cat.Chapters), orDefault(cat.Format, "未知"))})

	selected, err := selectChapters(cat.Chapters, req)
	if err != nil {
		return err
	}

	book := Book{Title: cat.Title, Author: cat.Author}
	if book.Title == "" {
		book.Title = state.Title
	}
	if book.Author == "" {
		book.Author = state.Author
	}

	format := "auto"
	total := len(selected)
	job.emit(Event{Type: "progress", Done: 0, Total: total, Title: "准备就绪"})

	var failed []string
	for i, ch := range selected {
		select {
		case <-ctx.Done():
			return fmt.Errorf("已取消")
		default:
		}

		content, css, err := fetchWithRetry(ctx, job, client, state.BookID, ch, &format)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("已取消")
			}
			// 单章失败不放弃整本：记下来继续，最后一并报告
			failed = append(failed, fmt.Sprintf("第%d章《%s》", ch.Idx, ch.Title))
			job.emit(Event{Type: "log", Message: fmt.Sprintf("✗ 第 %d 章跳过：%v", ch.Idx, err)})
			job.emit(Event{Type: "progress", Done: i + 1, Total: total, Title: "跳过 " + ch.Title})
			continue
		}
		if book.CSS == "" && css != "" {
			book.CSS = css
		}
		book.Chapters = append(book.Chapters, *content)
		job.emit(Event{
			Type:  "progress",
			Done:  i + 1,
			Total: total,
			Title: ch.Title,
		})
		time.Sleep(200 * time.Millisecond)
	}

	if len(book.Chapters) == 0 {
		return fmt.Errorf("所有章节都抓取失败，未生成文件")
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	name := safeFileName(book.Title) + ".epub"
	full := filepath.Join(outDir, name)
	f, err := os.Create(full)
	if err != nil {
		return err
	}
	writeErr := WriteEPUB(f, book)
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("写 EPUB 失败: %w", writeErr)
	}
	if closeErr != nil {
		return closeErr
	}

	info, _ := os.Stat(full)
	var size int64
	if info != nil {
		size = info.Size()
	}
	job.FileName = name
	msg := "导出完成"
	if len(failed) > 0 {
		msg = fmt.Sprintf("导出完成，但有 %d 章被跳过：%s", len(failed), strings.Join(failed, "、"))
	}
	job.emit(Event{Type: "done", File: name, Size: size, Message: msg, Done: total, Total: total})
	return nil
}

// fetchWithRetry 撞限流时退避重试（4s→8s→16s→32s→60s），并把请求间隔翻倍。
// 非限流错误立即返回，不做无谓重试。
func fetchWithRetry(ctx context.Context, job *Job, c *Client, bookID string,
	ch Chapter, format *string) (*ChapterContent, string, error) {
	const maxAttempts = 5
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		content, css, err := c.FetchChapter(bookID, ch, format)
		if err == nil {
			return content, css, nil
		}
		lastErr = err

		var rl *rateLimitError
		if !errors.As(err, &rl) {
			return nil, "", err
		}

		wait := time.Duration(1<<uint(attempt-1)) * 4 * time.Second
		if wait > 60*time.Second {
			wait = 60 * time.Second
		}
		interval := c.Penalize()
		job.emit(Event{Type: "log", Message: fmt.Sprintf(
			"第 %d 章被限流，%s 后重试（%d/%d），请求间隔调到 %s",
			ch.Idx, wait, attempt, maxAttempts, interval)})

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, "", fmt.Errorf("已取消")
		}
	}
	return nil, "", fmt.Errorf("重试 %d 次仍被限流: %w", maxAttempts, lastErr)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
