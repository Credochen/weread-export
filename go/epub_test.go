package main

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
)

func sampleBook() Book {
	return Book{
		Title:  "测试书",
		Author: "某作者",
		CSS:    "body { line-height: 1.7; }",
		Chapters: []ChapterContent{
			{
				Chapter: Chapter{UID: "1", Idx: 1, Title: "第一章 起"},
				HTML:    "<html><head><title>x</title></head><body><p>正文一</p></body></html>",
				Assets: []Asset{{
					Href:      "images/a.png",
					MediaType: "image/png",
					Data:      []byte("\x89PNG\r\n\x1a\nfake"),
				}},
			},
			{
				Chapter: Chapter{UID: "2", Idx: 2, Title: "第二章 承"},
				HTML:    "<p>没有 html 包裹的片段</p>",
			},
		},
	}
}

func readEntry(t *testing.T, zr *zip.Reader, name string) string {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", name, err)
		}
		return string(b)
	}
	t.Fatalf("EPUB 里缺少条目 %s", name)
	return ""
}

func TestWriteEPUB(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteEPUB(&buf, sampleBook()); err != nil {
		t.Fatalf("WriteEPUB 失败: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("生成的 ZIP 无法解析: %v", err)
	}

	// 1. mimetype 必须是第一个条目、STORED、内容正确、且不能带 extra 字段
	first := zr.File[0]
	if first.Name != "mimetype" {
		t.Fatalf("第一个条目是 %q，必须是 mimetype", first.Name)
	}
	if first.Method != zip.Store {
		t.Fatalf("mimetype 压缩方式 = %d，必须是 Store(0)", first.Method)
	}
	if len(first.Extra) != 0 {
		t.Fatalf("mimetype 条目带了 %d 字节 extra 字段，OCF 规范要求为空", len(first.Extra))
	}
	if got := readEntry(t, zr, "mimetype"); got != "application/epub+zip" {
		t.Fatalf("mimetype 内容 = %q", got)
	}

	// 2. 必需条目齐全
	for _, name := range []string{
		"META-INF/container.xml",
		"OEBPS/content.opf",
		"OEBPS/nav.xhtml",
		"OEBPS/toc.ncx",
		"OEBPS/styles/weread.css",
		"OEBPS/images/a.png",
		"OEBPS/text/chapter_0001.xhtml",
		"OEBPS/text/chapter_0002.xhtml",
	} {
		readEntry(t, zr, name)
	}

	// 3. OPF 里的标题、作者、章节引用
	opf := readEntry(t, zr, "OEBPS/content.opf")
	for _, want := range []string{"测试书", "某作者", `id="chapter_0001"`, `id="chapter_0002"`, `media-type="application/epub+zip"`} {
		if want == `media-type="application/epub+zip"` {
			continue // mimetype 不在 opf 里
		}
		if !strings.Contains(opf, want) {
			t.Fatalf("content.opf 里缺少 %q", want)
		}
	}

	// 4. 章节 XHTML 能正确抽出 body，且标题入 h1
	ch1 := readEntry(t, zr, "OEBPS/text/chapter_0001.xhtml")
	if !strings.Contains(ch1, "<p>正文一</p>") {
		t.Fatalf("第一章正文丢失: %s", ch1)
	}
	if !strings.Contains(ch1, "<h1>第一章 起</h1>") {
		t.Fatalf("第一章标题未写入 h1: %s", ch1)
	}
	if strings.Contains(ch1, "<html><head><title>x</title>") {
		t.Fatalf("第一章未剥离原始 head")
	}

	ch2 := readEntry(t, zr, "OEBPS/text/chapter_0002.xhtml")
	if !strings.Contains(ch2, "<p>没有 html 包裹的片段</p>") {
		t.Fatalf("无 body 包裹的片段被丢弃: %s", ch2)
	}

	// 5. nav 目录顺序
	nav := readEntry(t, zr, "OEBPS/nav.xhtml")
	if strings.Index(nav, "第一章") > strings.Index(nav, "第二章") {
		t.Fatalf("nav 目录顺序错误")
	}
}

func TestRewriteImageSources(t *testing.T) {
	srcMap := map[string]string{
		"a.png":   "../images/a.png",
		"a.png?v": "../images/a.png",
		"b.jpg":   "../images/b.jpg",
	}
	cases := []struct{ in, want string }{
		{
			`<img src="https://res.weread.qq.com/x/a.png"/>`,
			`<img src="../images/a.png"/>`,
		},
		{
			`<img src='https://res.weread.qq.com/x/b.jpg?foo=1'/>`,
			`<img src='../images/b.jpg'/>`,
		},
		{
			`<img src="https://other/c.png"/>`,
			`<img src="https://other/c.png"/>`,
		},
	}
	for _, c := range cases {
		if got := rewriteImageSources(c.in, srcMap); got != c.want {
			t.Fatalf("rewriteImageSources(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestTextToXHTML(t *testing.T) {
	got := textToXHTML("第一段\r\n\r\n第二段<危险>")
	if !strings.Contains(got, "<p>第一段</p>") || !strings.Contains(got, "<p>第二段&lt;危险&gt;</p>") {
		t.Fatalf("TXT 转换结果不对: %s", got)
	}
}

func TestSelectChapters(t *testing.T) {
	list := []Chapter{{Idx: 1}, {Idx: 2}, {Idx: 3}, {Idx: 4}, {Idx: 5}}

	all, _ := selectChapters(list, ExportRequest{Mode: "all"})
	if len(all) != 5 {
		t.Fatalf("all 应返回 5 章，得到 %d", len(all))
	}

	lim, _ := selectChapters(list, ExportRequest{Mode: "limit", Limit: 2})
	if len(lim) != 2 || lim[1].Idx != 2 {
		t.Fatalf("limit 结果不对: %+v", lim)
	}

	cus, err := selectChapters(list, ExportRequest{Mode: "custom", Spec: "1-2,5"})
	if err != nil {
		t.Fatalf("custom 解析失败: %v", err)
	}
	if len(cus) != 3 || cus[0].Idx != 1 || cus[2].Idx != 5 {
		t.Fatalf("custom 结果不对: %+v", cus)
	}

	cus2, _ := selectChapters(list, ExportRequest{Mode: "custom", Spec: "4-"})
	if len(cus2) != 2 || cus2[0].Idx != 4 {
		t.Fatalf("开放区间解析不对: %+v", cus2)
	}

	if _, err := selectChapters(list, ExportRequest{Mode: "custom", Spec: "abc"}); err == nil {
		t.Fatalf("非法范围应该报错")
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		"正常书名":             "正常书名",
		`a/b:c*d?"e"<f>|g`: "a_b_c_d_e_f_g",
		"":                 "weread-book",
	}
	for in, want := range cases {
		if got := safeFileName(in); got != want {
			t.Fatalf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
