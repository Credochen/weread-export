package main

import (
	"archive/zip"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
	"time"
)

// Book 待写入 EPUB 的完整书籍
type Book struct {
	Title    string
	Author   string
	CSS      string
	Chapters []ChapterContent
}

var bodyRe = regexp.MustCompile(`(?is)<body[^>]*>(.*?)</body>`)

func extractBody(source string) string {
	if m := bodyRe.FindStringSubmatch(source); m != nil {
		return m[1]
	}
	return source
}

func esc(s string) string { return html.EscapeString(s) }

func chapterXHTML(title, source string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="zh-CN">
<head><meta charset="utf-8"/><title>%s</title>
<link rel="stylesheet" type="text/css" href="../styles/weread.css"/></head>
<body><h1>%s</h1>
%s
</body></html>`, esc(title), esc(title), extractBody(source))
}

// WriteEPUB 生成 EPUB3。注意 mimetype 必须是第一个条目且不压缩。
func WriteEPUB(w io.Writer, b Book) error {
	bookID := func() string {
		sum := md5.Sum([]byte(b.Title + time.Now().String()))
		return "urn:uuid:" + hex.EncodeToString(sum[:])[:32]
	}()
	modTime := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	zw := zip.NewWriter(w)
	defer zw.Close()

	// 1. mimetype —— 第一条，STORED
	// 注意：不能设置 Modified，否则 Go 会写入扩展时间戳 extra 字段，
	// 严格的阅读器会因 mimetype 条目带 extra 而拒绝加载。
	mh := &zip.FileHeader{Name: "mimetype", Method: zip.Store}
	mw, err := zw.CreateHeader(mh)
	if err != nil {
		return err
	}
	if _, err := mw.Write([]byte("application/epub+zip")); err != nil {
		return err
	}

	add := func(name, content string) error {
		fw, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(fw, content)
		return err
	}

	if err := add("META-INF/container.xml", `<?xml version="1.0" encoding="utf-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`); err != nil {
		return err
	}

	n := len(b.Chapters)
	var nav, manifestCh, spine, ncxPoints strings.Builder
	for i := 1; i <= n; i++ {
		title := esc(b.Chapters[i-1].Title)
		href := fmt.Sprintf("text/chapter_%04d.xhtml", i)
		fmt.Fprintf(&nav, "      <li><a href=\"%s\">%s</a></li>\n", href, title)
		fmt.Fprintf(&manifestCh, "    <item id=\"chapter_%04d\" href=\"%s\" media-type=\"application/xhtml+xml\"/>\n", i, href)
		fmt.Fprintf(&spine, "    <itemref idref=\"chapter_%04d\"/>\n", i)
		fmt.Fprintf(&ncxPoints, `    <navPoint id="navPoint-%d" playOrder="%d">
      <navLabel><text>%s</text></navLabel>
      <content src="%s"/>
    </navPoint>
`, i, i, title, href)
	}

	// 图片资源去重
	seen := map[string]bool{}
	var assets []Asset
	for _, ch := range b.Chapters {
		for _, a := range ch.Assets {
			if seen[a.Href] {
				continue
			}
			seen[a.Href] = true
			assets = append(assets, a)
		}
	}
	var manifestAssets strings.Builder
	for i, a := range assets {
		fmt.Fprintf(&manifestAssets, "    <item id=\"asset_%04d\" href=\"%s\" media-type=\"%s\"/>\n", i+1, esc(a.Href), esc(a.MediaType))
	}

	opf := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="bookid">%s</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>zh-CN</dc:language>
    <meta property="dcterms:modified">%s</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="toc" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
    <item id="style" href="styles/weread.css" media-type="text/css"/>
%s%s  </manifest>
  <spine toc="toc">
%s  </spine>
</package>
`, esc(bookID), esc(b.Title), esc(b.Author), modTime, manifestCh.String(), manifestAssets.String(), spine.String())

	if err := add("OEBPS/content.opf", opf); err != nil {
		return err
	}

	navDoc := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="zh-CN">
<head><meta charset="utf-8"/><title>目录</title></head>
<body>
  <nav epub:type="toc" id="toc">
    <h1>目录</h1>
    <ol>
%s    </ol>
  </nav>
</body></html>
`, nav.String())
	if err := add("OEBPS/nav.xhtml", navDoc); err != nil {
		return err
	}

	ncx := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <head>
    <meta name="dtb:uid" content="%s"/>
    <meta name="dtb:depth" content="1"/>
    <meta name="dtb:totalPageCount" content="0"/>
    <meta name="dtb:maxPageNumber" content="0"/>
  </head>
  <docTitle><text>%s</text></docTitle>
  <navMap>
%s  </navMap>
</ncx>
`, esc(bookID), esc(b.Title), ncxPoints.String())
	if err := add("OEBPS/toc.ncx", ncx); err != nil {
		return err
	}

	css := b.CSS
	if strings.TrimSpace(css) == "" {
		css = "body { line-height: 1.7; margin: 0 1em; }\np { text-indent: 2em; margin: 0.6em 0; }\nh1 { font-size: 1.4em; text-indent: 0; }\nimg { max-width: 100%; }\n"
	}
	if err := add("OEBPS/styles/weread.css", css); err != nil {
		return err
	}

	for _, a := range assets {
		fw, err := zw.Create("OEBPS/" + a.Href)
		if err != nil {
			return err
		}
		if _, err := fw.Write(a.Data); err != nil {
			return err
		}
	}

	for i, ch := range b.Chapters {
		if err := add(fmt.Sprintf("OEBPS/text/chapter_%04d.xhtml", i+1),
			chapterXHTML(ch.Title, ch.HTML)); err != nil {
			return err
		}
	}
	return nil
}
