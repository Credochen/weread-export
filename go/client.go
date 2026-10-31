package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultUA   = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	baseURL     = "https://weread.qq.com"
	originURL   = "https://weread.qq.com"
	defaultHome = "https://weread.qq.com/"
)

// ---------- Cookie 存储 ----------

type cookieStore struct {
	mu   sync.Mutex
	vals map[string]string
}

// newCookieStore 支持两种输入：
//  1. 浏览器 DevTools 里复制的原始头：wr_vid=...; wr_skey=...
//  2. Cookie-Editor / EditThisCookie 导出的 JSON 数组（字段含 domain/name/value）
//
// JSON 模式下只保留 weread.qq.com 的条目，导出文件里往往夹着一堆 .qq.com 的统计
// Cookie，既没用又会把请求头撑大。
func newCookieStore(raw string) *cookieStore {
	cs := &cookieStore{vals: map[string]string{}}
	trimmed := strings.TrimSpace(raw)
	// 复制粘贴常带 BOM，会把 "[" 判断顶掉
	trimmed = strings.TrimPrefix(trimmed, "\ufeff")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return cs
	}

	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		if cs.loadJSON(trimmed) {
			return cs
		}
	}

	for _, part := range strings.Split(trimmed, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		cs.vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return cs
}

type jsonCookie struct {
	Domain string `json:"domain"`
	Name   string `json:"name"`
	Value  string `json:"value"`
}

// loadJSON 解析插件导出的 Cookie 数组，成功且至少命中一条才返回 true
func (c *cookieStore) loadJSON(s string) bool {
	var items []jsonCookie
	if err := json.Unmarshal([]byte(s), &items); err != nil {
		// 粘贴被截断（少了结尾的 ]）是最常见的失败原因，先尝试补全再解析一次
		if fixed, ok := repairTruncatedJSON(s); ok {
			if err2 := json.Unmarshal([]byte(fixed), &items); err2 == nil {
				goto parsed
			}
		}
		// 兼容 {"cookies":[...]} 这种包装
		var wrapper struct {
			Cookies []jsonCookie `json:"cookies"`
		}
		if err2 := json.Unmarshal([]byte(s), &wrapper); err2 != nil {
			return false
		}
		items = wrapper.Cookies
	}
parsed:

	for _, it := range items {
		if it.Name == "" {
			continue
		}
		if it.Domain != "" && !strings.HasSuffix(strings.TrimPrefix(it.Domain, "."), "weread.qq.com") {
			continue
		}
		c.vals[it.Name] = it.Value
	}
	return len(c.vals) > 0
}

func (c *cookieStore) header() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	parts := make([]string, 0, len(c.vals))
	for k, v := range c.vals {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// apply 只做「有值就更新」，绝不因为空值删除。
// 踩过的坑：renewal 鉴权失败时会回 Set-Cookie 把 wr_skey 置空，
// 早期版本照着写回本地，结果后续请求全变成未登录（-2010）。
func (c *cookieStore) apply(cookies []*http.Cookie) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ck := range cookies {
		if ck.Value != "" {
			c.vals[ck.Name] = ck.Value
		}
	}
}

// repairTruncatedJSON 处理粘贴时被截断的 JSON 数组：去掉尾部残缺项并补上 ]
func repairTruncatedJSON(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "[") || strings.HasSuffix(t, "]") {
		return "", false
	}
	if i := strings.LastIndex(t, "}"); i > 0 {
		return t[:i+1] + "]", true
	}
	return "", false
}

// ---------- 客户端 ----------

type Client struct {
	hc    *http.Client
	ua    string
	cs    *cookieStore
	token string

	mu    sync.Mutex
	delay time.Duration // 分片请求之间的间隔，撞限流后会翻倍
}

// rateLimitError 服务端触发风控/频率限制（-10102）时返回
type rateLimitError struct {
	Code string
	Msg  string
}

func (e *rateLimitError) Error() string {
	return fmt.Sprintf("被服务端限流（%s %s）", e.Code, e.Msg)
}

func NewClient(cookieString string) *Client {
	return &Client{
		hc:    &http.Client{Timeout: 40 * time.Second},
		ua:    defaultUA,
		cs:    newCookieStore(cookieString),
		delay: 150 * time.Millisecond,
	}
}

// pace 在每个分片请求后歇一下，别把接口打爆
func (c *Client) pace() {
	c.mu.Lock()
	d := c.delay
	c.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
}

// Penalize 撞到限流后把间隔翻倍（上限 3 秒）
func (c *Client) Penalize() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.delay *= 2
	if c.delay > 3*time.Second {
		c.delay = 3 * time.Second
	}
	return c.delay
}

func (c *Client) HasCookie() bool { return c.cs.header() != "" }

// CookieCount 解析到的 Cookie 条数
func (c *Client) CookieCount() int {
	c.cs.mu.Lock()
	defer c.cs.mu.Unlock()
	return len(c.cs.vals)
}

// CookieNames 已解析的 Cookie 名（排序后，便于排查缺哪个）
func (c *Client) CookieNames() []string {
	c.cs.mu.Lock()
	defer c.cs.mu.Unlock()
	names := make([]string, 0, len(c.cs.vals))
	for k := range c.cs.vals {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// CookieValue 读单个 Cookie 值（用于展示账号名等）
func (c *Client) CookieValue(name string) string {
	c.cs.mu.Lock()
	defer c.cs.mu.Unlock()
	return c.cs.vals[name]
}

func (c *Client) do(method, u string, body any, referer, accept string) ([]byte, error) {
	data, _, err := c.doOpt(method, u, body, referer, accept, true)
	return data, err
}

// doOpt 的 commit=false 用于「响应里的 Set-Cookie 不可信」的场景（如鉴权失败的续期）
func (c *Client) doOpt(method, u string, body any, referer, accept string, commit bool) ([]byte, []*http.Cookie, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return nil, nil, err
	}
	if referer == "" {
		referer = defaultHome
	}
	req.Header.Set("user-agent", c.ua)
	req.Header.Set("accept", accept)
	req.Header.Set("origin", originURL)
	req.Header.Set("referer", referer)
	if body != nil {
		req.Header.Set("content-type", "application/json;charset=UTF-8")
	}
	if h := c.cs.header(); h != "" {
		req.Header.Set("cookie", h)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	setCookies := resp.Cookies()
	if commit {
		c.cs.apply(setCookies)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("HTTP %d %s: %s", resp.StatusCode, u, truncate(string(data), 200))
	}
	return data, setCookies, nil
}

const acceptJSON = "application/json, text/plain, */*"
const acceptHTML = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"

func (c *Client) getText(u, referer string) (string, error) {
	b, err := c.do(http.MethodGet, u, nil, referer, acceptHTML)
	return string(b), err
}

func (c *Client) postJSON(u string, body any, referer string) (map[string]any, error) {
	b, err := c.do(http.MethodPost, u, body, referer, acceptJSON)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("响应不是 JSON: %s", truncate(string(b), 160))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Renew 续期登录态。失败是常态（该接口对 Web Cookie 往往回 -2013），
// 此时绝不能把响应里的 Set-Cookie 写回本地，否则会把有效登录态清掉。
func (c *Client) Renew() bool {
	data, setCookies, err := c.doOpt(http.MethodPost, baseURL+"/web/login/renewal",
		map[string]any{"rq": "%2Fweb%2Fbook%2Fread", "ql": false}, defaultHome, acceptJSON, false)
	if err != nil {
		return false
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return false
	}
	if code := str(out["errCode"]); code != "" && code != "0" {
		return false
	}
	succ, _ := out["succ"].(float64)
	if succ == 0 {
		return false
	}
	c.cs.apply(setCookies)
	return true
}

// ---------- 阅读页状态 ----------

type ReaderState struct {
	BookID   string
	Title    string
	Author   string
	PSVTS    string
	PCLTS    string
	Token    string
	UserName string
}

var stateMarker = []byte("window.__INITIAL_STATE__")

// GetReaderState 从阅读页扒 __INITIAL_STATE__
func (c *Client) GetReaderState(readerURL string) (*ReaderState, error) {
	html, err := c.getText(readerURL, readerURL)
	if err != nil {
		return nil, err
	}

	idx := bytes.Index([]byte(html), stateMarker)
	if idx < 0 {
		return nil, fmt.Errorf("页面里没找到 __INITIAL_STATE__（可能是 Cookie 失效被重定向了）")
	}
	rest := html[idx+len(stateMarker):]
	rest = strings.TrimLeft(rest, " \t\r\n=")

	dec := json.NewDecoder(strings.NewReader(rest))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("解析 __INITIAL_STATE__ 失败: %w", err)
	}

	reader := asMap(root["reader"])
	info := asMap(reader["bookInfo"])
	st := &ReaderState{
		BookID:   str(info["bookId"]),
		Title:    str(info["title"]),
		Author:   str(info["author"]),
		PSVTS:    str(reader["psvts"]),
		PCLTS:    str(reader["pclts"]),
		Token:    str(reader["token"]),
		UserName: str(asMap(root["user"])["name"]),
	}
	if st.BookID == "" {
		return nil, fmt.Errorf("页面里没找到 bookId")
	}
	c.token = st.Token
	return st, nil
}

func readerURLFor(bookID, chapterUID string) string {
	u := baseURL + "/web/reader/" + WereadE(bookID)
	if chapterUID != "" {
		u += "k" + WereadE(chapterUID)
	}
	return u
}

// ---------- 章节目录 ----------

type Chapter struct {
	UID       string `json:"uid"`
	Idx       int    `json:"idx"`
	Title     string `json:"title"`
	WordCount int    `json:"wordCount"`
	Paid      int    `json:"paid"`
	Tar       string `json:"-"`
}

type Catalog struct {
	Title    string
	Author   string
	Format   string
	Chapters []Chapter
}

// GetCatalog 拉目录。未登录会返回 errCode=-2010
func (c *Client) GetCatalog(bookID, referer string) (*Catalog, error) {
	out, err := c.postJSON(baseURL+"/web/book/chapterInfos",
		map[string]any{"bookIds": []string{bookID}}, referer)
	if err != nil {
		return nil, err
	}
	if code := str(out["errCode"]); code != "" && code != "0" {
		return nil, fmt.Errorf("errCode=%s %s", code, str(out["errMsg"]))
	}

	var records []any
	switch {
	case asSlice(out["data"]) != nil:
		records = asSlice(out["data"])
	case asSlice(out["books"]) != nil:
		records = asSlice(out["books"])
	default:
		records = []any{out}
	}

	var record map[string]any
	for _, r := range records {
		m := asMap(r)
		if m == nil {
			continue
		}
		cid := str(m["bookId"])
		if cid == "" {
			cid = str(asMap(m["book"])["bookId"])
		}
		if cid == bookID {
			record = m
			break
		}
	}
	if record == nil {
		return nil, fmt.Errorf("目录里没找到 bookId=%s 的记录", bookID)
	}

	raw := asSlice(record["updated"])
	if raw == nil {
		raw = asSlice(record["chapterInfos"])
	}
	if raw == nil {
		raw = asSlice(record["chapters"])
	}

	cat := &Catalog{
		Title:  str(asMap(record["book"])["title"]),
		Author: str(asMap(record["book"])["author"]),
		Format: str(record["format"]),
	}
	if cat.Title == "" {
		cat.Title = str(record["title"])
	}
	if cat.Author == "" {
		cat.Author = str(record["author"])
	}

	idx := 0
	for _, r := range raw {
		m := asMap(r)
		if m == nil || str(m["chapterUid"]) == "" {
			continue
		}
		ch := Chapter{
			UID:       str(m["chapterUid"]),
			Title:     str(m["title"]),
			WordCount: num(m["wordCount"]),
			Paid:      num(m["paid"]),
			Tar:       str(m["tar"]),
		}
		if ch.WordCount <= 0 || ch.Title == "封面" {
			continue
		}
		idx++
		ch.Idx = idx
		cat.Chapters = append(cat.Chapters, ch)
	}
	if len(cat.Chapters) == 0 {
		return nil, fmt.Errorf("目录为空（可能没有该书的阅读权益）")
	}
	return cat, nil
}

// ---------- 正文分片 ----------

func contentParams(bookID, chapterUID, psvts string, style bool) map[string]any {
	ctValue := time.Now().Unix()
	ct := strconv.FormatInt(ctValue, 10)
	// 协议约束：pc 不能等于 ps，否则服务端直接回 {}
	if WereadE(ct) == psvts {
		ct = strconv.FormatInt(ctValue+1, 10)
	}
	st := 0
	if style {
		st = 1
	}
	p := map[string]any{
		"b":           WereadE(bookID),
		"c":           WereadE(chapterUID),
		"r":           strconv.Itoa(rand.Intn(10000) * rand.Intn(10000)),
		"ct":          ct,
		"ps":          psvts,
		"pc":          WereadE(ct),
		"sc":          1,
		"prevChapter": false,
		"st":          st,
	}
	p["s"] = WereadSign(SortedQuery(p))
	return p
}

func (c *Client) shard(bookID, chapterUID, psvts, endpoint, referer string, style bool) (string, error) {
	params := contentParams(bookID, chapterUID, psvts, style)
	b, err := c.do(http.MethodPost, baseURL+endpoint, params, referer, acceptJSON)
	if err != nil {
		return "", err
	}
	text := string(b)
	if text == "{}" || text == "" {
		return "", fmt.Errorf("%s 返回空（签名或权益问题）", endpoint)
	}
	if err := shardError(text); err != nil {
		return "", err
	}
	c.pace()
	return text, nil
}

// shardError 识别「本该是分片、实际是 JSON 错误体」的情况。
// 踩过的坑：被限流时服务端回 {"errcode":-10102,"errmsg":"Hit ..."}，
// 旧逻辑当正文去解码，最后报「分片 MD5 不匹配」，完全误导。
func shardError(text string) error {
	if !strings.HasPrefix(strings.TrimSpace(text), "{") {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return nil
	}
	code := str(m["errcode"])
	if code == "" {
		code = str(m["errCode"])
	}
	if code == "" {
		return nil // 这是 TXT 格式的探测响应，不是错误
	}
	msg := str(m["errmsg"])
	if msg == "" {
		msg = str(m["errMsg"])
	}
	if code == "-10102" || code == "-1012" {
		return &rateLimitError{Code: code, Msg: msg}
	}
	return fmt.Errorf("服务端拒绝 %s %s", code, msg)
}

type Asset struct {
	Href      string
	MediaType string
	Data      []byte
}

type ChapterContent struct {
	Chapter
	HTML   string
	Assets []Asset
}

// FetchChapter 抓一章：分片 + 解码 + 图片
func (c *Client) FetchChapter(bookID string, ch Chapter, format *string) (*ChapterContent, string, error) {
	referer := readerURLFor(bookID, ch.UID)
	st, err := c.GetReaderState(referer)
	if err != nil {
		return nil, "", fmt.Errorf("取会话参数失败: %w", err)
	}
	if st.PSVTS == "" {
		return nil, "", fmt.Errorf("psvts 为空")
	}

	cc := &ChapterContent{Chapter: ch}
	var css string

	if *format == "txt" {
		html, err := c.fetchTXT(bookID, ch.UID, st.PSVTS, referer)
		if err != nil {
			return nil, "", err
		}
		cc.HTML = html
		return cc, "", nil
	}

	e0, err := c.shard(bookID, ch.UID, st.PSVTS, "/web/book/chapter/e_0", referer, false)
	if err != nil {
		return nil, "", err
	}
	// e_0 回的是 JSON 说明这本书是 TXT 格式
	if strings.HasPrefix(e0, "{") && strings.Contains(e0, "\"bookId\"") {
		*format = "txt"
		html, terr := c.fetchTXT(bookID, ch.UID, st.PSVTS, referer)
		if terr != nil {
			return nil, "", terr
		}
		cc.HTML = html
		return cc, "", nil
	}
	*format = "epub"

	e1, err := c.shard(bookID, ch.UID, st.PSVTS, "/web/book/chapter/e_1", referer, false)
	if err != nil {
		return nil, "", err
	}
	e3, err := c.shard(bookID, ch.UID, st.PSVTS, "/web/book/chapter/e_3", referer, false)
	if err != nil {
		return nil, "", err
	}
	html, err := DecodeChapter(e0, e1, e3)
	if err != nil {
		return nil, "", fmt.Errorf("解码失败: %w", err)
	}

	if e2, err := c.shard(bookID, ch.UID, st.PSVTS, "/web/book/chapter/e_2", referer, true); err == nil {
		if body, err := CheckedBody(e2); err == nil {
			if s, err := DecodePayload(body); err == nil {
				css = s
			}
		}
	}

	assets, srcMap, err := c.fetchAssets(ch.Tar, referer)
	if err == nil && len(srcMap) > 0 {
		html = rewriteImageSources(html, srcMap)
	}
	cc.HTML = html
	cc.Assets = assets
	return cc, css, nil
}

func (c *Client) fetchTXT(bookID, chapterUID, psvts, referer string) (string, error) {
	t0, err := c.shard(bookID, chapterUID, psvts, "/web/book/chapter/t_0", referer, false)
	if err != nil {
		return "", err
	}
	t1 := ""
	if v, err := c.shard(bookID, chapterUID, psvts, "/web/book/chapter/t_1", referer, false); err == nil {
		t1 = v
	}
	plain, err := DecodeChapter(t0, t1, "")
	if err != nil {
		return "", fmt.Errorf("TXT 解码失败: %w", err)
	}
	return textToXHTML(plain), nil
}

var htmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func textToXHTML(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	var sb strings.Builder
	sb.WriteString("<div>")
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		sb.WriteString("<p>" + htmlEscape.Replace(line) + "</p>")
	}
	sb.WriteString("</div>")
	return sb.String()
}

// ---------- 图片资源 ----------

func (c *Client) fetchAssets(tarURL, referer string) ([]Asset, map[string]string, error) {
	if tarURL == "" {
		return nil, nil, nil
	}
	if !strings.HasPrefix(tarURL, "http") {
		tarURL = baseURL + "/" + strings.TrimPrefix(tarURL, "/")
	}
	raw, err := c.do(http.MethodGet, tarURL, nil, referer, "*/*")
	if err != nil {
		return nil, nil, err
	}

	var assets []Asset
	srcMap := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			continue
		}
		ext, mediaType := imageExt(data)
		if mediaType == "" {
			continue
		}
		stem := path.Base(hdr.Name)
		filename := stem
		if !strings.HasSuffix(strings.ToLower(filename), ext) {
			filename += ext
		}
		href := "images/" + filename
		assets = append(assets, Asset{Href: href, MediaType: mediaType, Data: data})
		srcMap[stem] = "../" + href
		srcMap[filename] = "../" + href
	}
	return assets, srcMap, nil
}

func imageExt(b []byte) (string, string) {
	switch {
	case len(b) > 8 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return ".png", "image/png"
	case len(b) > 3 && bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return ".jpg", "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")) || bytes.HasPrefix(b, []byte("GIF89a")):
		return ".gif", "image/gif"
	case len(b) > 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP":
		return ".webp", "image/webp"
	}
	return "", ""
}

// rewriteImageSources 把正文里的远程图片 src 改成 EPUB 内部相对路径。
// 不用正则：Go 的 RE2 不支持反向引用，只能手写扫描配对引号。
func rewriteImageSources(source string, srcMap map[string]string) string {
	if len(srcMap) == 0 {
		return source
	}
	var sb strings.Builder
	i := 0
	for {
		idx := strings.Index(source[i:], "src=")
		if idx < 0 {
			sb.WriteString(source[i:])
			break
		}
		idx += i
		sb.WriteString(source[i:idx])

		rest := source[idx+4:]
		if rest == "" {
			sb.WriteString("src=")
			break
		}
		quote := rest[0]
		if quote != '"' && quote != '\'' {
			sb.WriteString("src=")
			i = idx + 4
			continue
		}
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			sb.WriteString(source[idx:])
			break
		}
		original := source[idx : idx+4+1+end+1]
		src := rest[1 : 1+end]
		if k := strings.IndexByte(src, '?'); k >= 0 {
			src = src[:k]
		}
		key := path.Base(strings.ReplaceAll(src, "\\", "/"))
		if href, ok := srcMap[key]; ok {
			sb.WriteString("src=" + string(quote) + href + string(quote))
		} else {
			sb.WriteString(original)
		}
		i = idx + 4 + 1 + end + 1
	}
	return sb.String()
}

// ---------- JSON 辅助 ----------

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(t)
	}
}

func num(v any) int {
	n, _ := strconv.Atoi(str(v))
	return n
}

// 供 job 使用
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
