package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type server struct {
	mgr    *Manager
	outDir string
}

// ---------- 接口 ----------

type inspectReq struct {
	Cookie    string `json:"cookie"`
	ReaderURL string `json:"readerUrl"`
	BookID    string `json:"bookId"`
}

type inspectResp struct {
	OK        bool      `json:"ok"`
	Message   string    `json:"message"`
	BookID    string    `json:"bookId"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Format    string    `json:"format"`
	LoggedIn  bool      `json:"loggedIn"`
	Account   string    `json:"account"`
	Chapters  []Chapter `json:"chapters"`
	PaidCount int       `json:"paidCount"`

	// 诊断用：解析到几条 Cookie、分别是哪些。
	// 报 -2010 时先看这个——0 条说明粘贴内容有问题，不是账号问题。
	CookieCount int      `json:"cookieCount"`
	CookieNames []string `json:"cookieNames"`
}

func (s *server) handleInspect(w http.ResponseWriter, r *http.Request) {
	var req inspectReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, inspectResp{Message: "请求体解析失败"})
		return
	}
	if strings.TrimSpace(req.Cookie) == "" {
		writeJSON(w, 200, inspectResp{Message: "请先填写 Cookie"})
		return
	}
	if req.ReaderURL == "" && req.BookID == "" {
		writeJSON(w, 200, inspectResp{Message: "请填写阅读页链接或 bookId"})
		return
	}

	client := NewClient(req.Cookie)
	resp := inspectResp{CookieCount: client.CookieCount(), CookieNames: client.CookieNames()}
	if resp.CookieCount == 0 {
		resp.Message = "没有解析出任何 Cookie。请粘贴完整的 Cookie-Editor JSON 数组，或 wr_vid=...; wr_skey=... 形式的请求头。"
		writeJSON(w, 200, resp)
		return
	}
	client.Renew()

	readerURL := req.ReaderURL
	if readerURL == "" {
		readerURL = readerURLFor(strings.TrimSpace(req.BookID), "")
	}

	state, err := client.GetReaderState(readerURL)
	if err != nil {
		resp.Message = err.Error()
		writeJSON(w, 200, resp)
		return
	}

	resp.BookID = state.BookID
	resp.Title = state.Title
	resp.Author = state.Author
	resp.LoggedIn = state.UserName != ""
	resp.Account = state.UserName

	cat, err := client.GetCatalog(state.BookID, readerURL)
	if err != nil {
		resp.Message = err.Error()
		if !resp.LoggedIn {
			resp.Message = "Cookie 没被认可（" + err.Error() + "）。已解析到 " +
				itoa(resp.CookieCount) + " 条 Cookie，检查是否缺 wr_vid / wr_skey / wr_rt。"
		}
		writeJSON(w, 200, resp)
		return
	}

	// 目录接口需要鉴权，能拿到目录就说明登录态有效。
	// （阅读页的 __INITIAL_STATE__.user.name 有时是空的，不能作为判据）
	resp.OK = true
	resp.LoggedIn = true
	if resp.Account == "" {
		if v := client.CookieValue("wr_name"); v != "" {
			if d, err := url.QueryUnescape(v); err == nil {
				resp.Account = d
			} else {
				resp.Account = v
			}
		}
	}
	resp.Format = cat.Format
	resp.Chapters = cat.Chapters
	if cat.Title != "" {
		resp.Title = cat.Title
	}
	if cat.Author != "" {
		resp.Author = cat.Author
	}
	for _, ch := range cat.Chapters {
		if ch.Paid == 1 {
			resp.PaidCount++
		}
	}
	resp.Message = fmt.Sprintf("识别成功，共 %d 章", len(cat.Chapters))
	writeJSON(w, 200, resp)
}

func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	var req ExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "请求体解析失败"})
		return
	}
	if strings.TrimSpace(req.Cookie) == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "缺少 Cookie"})
		return
	}
	job, err := s.mgr.Start(req, s.outDir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jobId": job.ID})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	job := s.mgr.get(r.PathValue("id"))
	if job == nil {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, snapshot := job.subscribe()
	defer job.unsubscribe(ch)

	for _, e := range snapshot {
		if writeSSE(w, e) != nil {
			return
		}
	}
	flusher.Flush()
	if n := len(snapshot); n > 0 {
		if last := snapshot[n-1]; last.Type == "done" || last.Type == "error" {
			return
		}
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case e, open := <-ch:
			if !open {
				return
			}
			if writeSSE(w, e) != nil {
				return
			}
			flusher.Flush()
			if e.Type == "done" || e.Type == "error" {
				return
			}
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	job := s.mgr.get(r.PathValue("id"))
	if job == nil || job.FileName == "" {
		http.Error(w, "文件还没生成", http.StatusNotFound)
		return
	}
	full := filepath.Join(s.outDir, job.FileName)
	if !fileExists(full) {
		http.Error(w, "文件已不存在", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename*=UTF-8''%s`, urlEncode(job.FileName)))
	http.ServeFile(w, r, full)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	job := s.mgr.get(r.PathValue("id"))
	if job == nil {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	writeJSON(w, 200, job.snapshot())
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	job := s.mgr.get(r.PathValue("id"))
	if job == nil {
		http.Error(w, "任务不存在", http.StatusNotFound)
		return
	}
	if job.cancel != nil {
		job.cancel()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

func itoa(n int) string { return strconv.Itoa(n) }

func urlEncode(s string) string {
	var sb strings.Builder
	for _, b := range []byte(s) {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') ||
			b == '-' || b == '_' || b == '.' || b == '~' {
			sb.WriteByte(b)
		} else {
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "selftest" {
		if RunSelfTest() {
			os.Exit(0)
		}
		os.Exit(1)
	}

	port := flag.Int("port", 8791, "监听端口")
	outDir := flag.String("out", "out", "EPUB 输出目录")
	noOpen := flag.Bool("no-open", false, "不自动打开浏览器")
	flag.Parse()

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("创建输出目录失败: %v", err)
	}

	s := &server{mgr: NewManager(), outDir: *outDir}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/inspect", s.handleInspect)
	mux.HandleFunc("POST /api/export", s.handleExport)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleStatus)
	mux.HandleFunc("GET /api/jobs/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/jobs/{id}/file", s.handleDownload)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancel)
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	url := fmt.Sprintf("http://%s", addr)

	fmt.Println()
	fmt.Println("  微信读书导出器")
	fmt.Printf("  → %s\n", url)
	fmt.Printf("  → 输出目录 %s\n", *outDir)
	fmt.Println("  按 Ctrl+C 退出")
	fmt.Println()

	if !*noOpen {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(url)
		}()
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}
