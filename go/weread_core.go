// 微信读书 Web 端核心算法（Go 移植）
//
// 移植自 finlater/weread.koplugin 的 scripts/fetch_weread_epub.py，
// 只包含三类"硬骨头"：ID 编码、请求签名、正文分片解码。
// 其余部分（HTTP 请求、EPUB 打包）是常规活，按需自己接。
//
// 运行自检：go run weread_core.go
package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// ---------- 1. ID 编码 ----------

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// WereadE 把 bookId / chapterUid / 时间戳编码成微信读书内部 ID。
// 阅读页 URL、请求参数 b/c/pc 都用它。
func WereadE(value string) string {
	h := md5Hex(value)
	result := h[:3]

	var chunks []string
	isNumeric := value != "" && strings.IndexFunc(value, func(r rune) bool {
		return r < '0' || r > '9'
	}) < 0

	if isNumeric {
		// 纯数字：按 9 位分段转十六进制
		for i := 0; i < len(value); i += 9 {
			end := i + 9
			if end > len(value) {
				end = len(value)
			}
			n, _ := strconv.ParseInt(value[i:end], 10, 64)
			chunks = append(chunks, strconv.FormatInt(n, 16))
		}
		result += "3"
	} else {
		// 非数字：逐字符码点转十六进制后拼接（不补零）
		var sb strings.Builder
		for _, r := range value {
			sb.WriteString(strconv.FormatInt(int64(r), 16))
		}
		chunks = []string{sb.String()}
		result += "4"
	}

	result += "2" + h[len(h)-2:]

	for i, chunk := range chunks {
		result += fmt.Sprintf("%02x", len(chunk)) + chunk
		if i < len(chunks)-1 {
			result += "g"
		}
	}

	if len(result) < 20 {
		result += h[:20-len(result)]
	}

	result += md5Hex(result)[:3]
	return result
}

// ---------- 2. 请求签名 ----------

// WereadSign 对"已排序的 query 串"求签名，结果放进参数 s。
// 注意：s 必须在其余字段都拼好之后再算。
func WereadSign(query string) string {
	const seed int64 = 0x15051505
	a, b := seed, seed
	length := len(query)
	i := length - 1

	for i > 0 {
		a = (a ^ (int64(query[i]) << ((length - i) % 30))) & 0x7FFFFFFF
		b = (b ^ (int64(query[i-1]) << (i % 30))) & 0x7FFFFFFF
		i -= 2
	}

	return strconv.FormatInt(a+b, 16)
}

// SortedQuery 按键名字典序拼接 query，值按 JS 语义转换（true/false/null），
// 再做全量 percent-encode（空格用 %20，不是 +）。
func SortedQuery(params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+percentEncode(jsString(params[k])))
	}
	return strings.Join(parts, "&")
}

func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// percentEncode 等价于 Python 的 urllib.parse.quote(s, safe=”)
func percentEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// WebAppID 由 User-Agent 推导浏览器 id，进度上报接口用。
func WebAppID(userAgent string) string {
	parts := strings.Split(userAgent, " ")
	if len(parts) > 12 {
		parts = parts[:12]
	}
	var prefix strings.Builder
	for _, p := range parts {
		prefix.WriteString(strconv.Itoa(len(p) % 10))
	}

	var value int64
	for _, r := range userAgent {
		value = (0x83*value + int64(r)) & 0x7FFFFFFF
	}
	return fmt.Sprintf("wb%sh%d", prefix.String(), value)
}

// ---------- 3. 正文分片解码 ----------

// CheckedBody 校验分片：前 32 字符是 body 的 MD5 大写，不符即报文损坏。
func CheckedBody(resp string) (string, error) {
	if len(resp) <= 32 {
		return "", nil
	}
	expected := resp[:32]
	body := resp[32:]
	actual := strings.ToUpper(md5Hex(body))
	if actual != expected {
		return "", fmt.Errorf("分片 MD5 不匹配: expected=%s actual=%s", expected, actual)
	}
	return body, nil
}

// swapPositions 从密文尾部推导出做过交换的位置对。
// 注意 int(bin(ord(ch))[2:], 4) —— 把二进制字符串当四进制数解析。
func swapPositions(encoded string) []int {
	length := len(encoded)
	if length < 4 {
		return nil
	}
	if length < 11 {
		return []int{0, 2}
	}

	n := (length + 9) / 10
	if n > 4 {
		n = 4
	}

	var tmp strings.Builder
	for i := length - 1; i > length-n-1; i-- {
		binary := strconv.FormatInt(int64(encoded[i]), 2)
		v, _ := strconv.ParseInt(binary, 4, 64)
		tmp.WriteString(strconv.FormatInt(v, 10))
	}
	digits := tmp.String()

	m := length - n - 2
	step := len(strconv.Itoa(m))

	var result []int
	for i := 0; len(result) < 10 && i+step < len(digits); i += step {
		v1, _ := strconv.Atoi(digits[i : i+step])
		result = append(result, v1%m)
		v2, _ := strconv.Atoi(digits[i+1 : i+1+step])
		result = append(result, v2%m)
	}
	return result
}

// reverseSwaps 逆序两两交换，还原本体。
func reverseSwaps(encoded string, positions []int) string {
	chars := []byte(encoded)
	for i := len(positions) - 1; i > 0; i -= 2 {
		for _, k := range []int{1, 0} {
			left := positions[i] + k
			right := positions[i-1] + k
			if left < len(chars) && right < len(chars) {
				chars[left], chars[right] = chars[right], chars[left]
			}
		}
	}
	return string(chars)
}

// repairUTF8 把按 latin-1 视角拿到的字节流手工重组成 UTF-8 字符串。
func repairUTF8(b []byte) string {
	var out strings.Builder
	i := 0
	for i < len(b) {
		b0 := b[i]
		switch {
		case b0 >= 0xC0 && b0 <= 0xDF && i+1 < len(b) && b[i+1] >= 0x80 && b[i+1] <= 0xBF:
			out.WriteRune(rune((int(b0&0x1F) << 6) | int(b[i+1]&0x3F)))
			i += 2
		case b0 >= 0xE0 && b0 <= 0xEF && i+2 < len(b) &&
			b[i+1] >= 0x80 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF:
			out.WriteRune(rune((int(b0&0x0F) << 12) | (int(b[i+1]&0x3F) << 6) | int(b[i+2]&0x3F)))
			i += 3
		case b0 >= 0xF0 && b0 <= 0xF7 && i+3 < len(b) &&
			b[i+1] >= 0x80 && b[i+1] <= 0xBF && b[i+2] >= 0x80 && b[i+2] <= 0xBF &&
			b[i+3] >= 0x80 && b[i+3] <= 0xBF:
			out.WriteRune(rune((int(b0&0x07) << 18) | (int(b[i+1]&0x3F) << 12) |
				(int(b[i+2]&0x3F) << 6) | int(b[i+3]&0x3F)))
			i += 4
		default:
			// 非法起始字节：按 latin-1 语义原样保留为单个 rune（对齐 Python 实现的 chr(b)）
			out.WriteRune(rune(b0))
			i++
		}
	}
	return out.String()
}

// DecodePayload 解一片（或多片拼接后的）密文：去首字符 -> 换位还原 -> base64 -> UTF-8 修复。
func DecodePayload(payload string) (string, error) {
	if payload == "" {
		return "", nil
	}
	if len(payload) < 1 {
		return "", fmt.Errorf("payload 为空")
	}
	body := payload[1:] // 去掉首字符
	reordered := reverseSwaps(body, swapPositions(body))

	// base64-url 转标准 base64，并剔除非法字符
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == '_':
			return r
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '+', r == '/':
			return r
		default:
			return -1
		}
	}, reordered)
	cleaned = strings.ReplaceAll(cleaned, "-", "+")
	cleaned = strings.ReplaceAll(cleaned, "_", "/")
	if pad := len(cleaned) % 4; pad != 0 {
		cleaned += strings.Repeat("=", 4-pad)
	}

	raw, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	return repairUTF8(raw), nil
}

// DecodeChapter EPUB 正文：e_0 + e_1 + e_3 三片拼接后统一解码。
func DecodeChapter(e0, e1, e3 string) (string, error) {
	var payload string
	for _, shard := range []string{e0, e1, e3} {
		body, err := CheckedBody(shard)
		if err != nil {
			return "", err
		}
		payload += body
	}
	return DecodePayload(payload)
}

// ---------- 4. 请求参数组装 ----------

// ContentParams 造章节内容请求体。psvts 取自阅读页 __INITIAL_STATE__.reader.psvts。
// style=true 时 st=1（仅 e_2 样式分片用）。
func ContentParams(bookID, chapterUID, psvts, ct string, style bool) map[string]any {
	st := 0
	if style {
		st = 1
	}
	params := map[string]any{
		"b":           WereadE(bookID),
		"c":           WereadE(chapterUID),
		"r":           "0",
		"ct":          ct,
		"ps":          psvts,
		"pc":          WereadE(ct),
		"sc":          1,
		"prevChapter": false,
		"st":          st,
	}
	params["s"] = WereadSign(SortedQuery(params))
	return params
}

// ---------- 自检 ----------

// RunSelfTest 打印算法自检结果，返回是否全部通过。
// 命令行入口：weread-export selftest
func RunSelfTest() bool {
	cases := []struct{ in, want string }{
		{"43208843", "c9c321c07293508bc9c79df"},
		{"2", "c81322c012c81e728d9d180"},
		{"119", "07e323f027707e1cd7dc674"},
	}
	fmt.Println("[1] WereadE")
	ok := true
	for _, c := range cases {
		got := WereadE(c.in)
		mark := "OK "
		if got != c.want {
			mark, ok = "FAIL", false
		}
		fmt.Printf("    %s e(%s) = %s\n", mark, c.in, got)
	}

	ua := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	fmt.Println("[2] WebAppID")
	fmt.Printf("    got  = %s\n    want = wb115321887466h529830856\n", WebAppID(ua))

	fmt.Println("[3] SortedQuery + WereadSign")
	p := map[string]any{
		"b": "x", "c": "y", "r": "1", "ct": "1700000000",
		"ps": "z", "pc": "q", "sc": 1, "prevChapter": false, "st": 0,
	}
	q := SortedQuery(p)
	fmt.Printf("    query = %s\n", q)
	fmt.Printf("    sign  = %s (want 1f5e5484)\n", WereadSign(q))

	fmt.Println("[4] swapPositions")
	fmt.Printf("    swapPositions(\"abcdefghijklmnop\") = %v (want [5 1 4 5 4 8])\n",
		swapPositions("abcdefghijklmnop"))

	if !ok {
		fmt.Println("\n结论：ID 编码自检未通过")
		return false
	}
	fmt.Println("\n结论：ID 编码自检通过")
	return true
}
