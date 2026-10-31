package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCookieStoreRawHeader(t *testing.T) {
	cs := newCookieStore("wr_vid=1; wr_skey=abc; wr_rt=x%40y")
	if got := cs.vals["wr_vid"]; got != "1" {
		t.Fatalf("wr_vid = %q", got)
	}
	if got := cs.vals["wr_rt"]; got != "x%40y" {
		t.Fatalf("wr_rt 应原样保留百分号编码，得到 %q", got)
	}
	if n := len(cs.vals); n != 3 {
		t.Fatalf("应解析 3 条，得到 %d", n)
	}
}

func TestCookieStoreJSONExport(t *testing.T) {
	// Cookie-Editor / EditThisCookie 导出的数组格式
	raw := `[
	  {"domain":".qq.com","name":"pgv_pvid","value":"7201222469","httpOnly":false},
	  {"domain":".qq.com","name":"RK","value":"nJwX+WJAUb","httpOnly":false},
	  {"domain":".weread.qq.com","name":"wr_vid","value":"12345678","httpOnly":true},
	  {"domain":".weread.qq.com","name":"wr_skey","value":"FAKEskey01","httpOnly":true},
	  {"domain":".weread.qq.com","name":"wr_rt","value":"o2ux__Bvoc%4019ygt","httpOnly":true},
	  {"domain":"weread.qq.com","name":"wr_gid","value":"100000001","httpOnly":false}
	]`

	cs := newCookieStore(raw)

	for _, name := range []string{"wr_vid", "wr_skey", "wr_rt", "wr_gid"} {
		if cs.vals[name] == "" {
			t.Fatalf("缺少 %s", name)
		}
	}

	// .qq.com 的统计 Cookie 必须被过滤掉，不能塞进请求头
	for _, name := range []string{"pgv_pvid", "RK"} {
		if _, ok := cs.vals[name]; ok {
			t.Fatalf("%s 不该被带进 weread 请求", name)
		}
	}

	if got := cs.vals["wr_rt"]; got != "o2ux__Bvoc%4019ygt" {
		t.Fatalf("wr_rt 应原样保留，得到 %q", got)
	}

	h := cs.header()
	if !strings.Contains(h, "wr_vid=12345678") {
		t.Fatalf("header 拼装异常: %s", h)
	}
	if strings.Contains(h, "pgv_pvid") {
		t.Fatalf("header 里混进了其他站点的 Cookie: %s", h)
	}
}

func TestCookieStoreWrappedJSON(t *testing.T) {
	raw := `{"cookies":[{"domain":".weread.qq.com","name":"wr_vid","value":"42"}]}`
	cs := newCookieStore(raw)
	if cs.vals["wr_vid"] != "42" {
		t.Fatalf("包装格式解析失败: %+v", cs.vals)
	}
}

func TestCookieStoreMalformedFallsBack(t *testing.T) {
	// 看起来像 JSON 但其实是坏的，不应崩溃，应回退成普通 k=v 解析
	cs := newCookieStore("[broken; wr_vid=7")
	if len(cs.vals) == 0 && cs.vals["wr_vid"] != "7" {
		t.Fatalf("回退解析失败: %+v", cs.vals)
	}
}

func TestCookieStoreBOM(t *testing.T) {
	// 从某些编辑器复制会带 BOM，旧逻辑会因此不走 JSON 分支
	raw := "\ufeff[{\"domain\":\".weread.qq.com\",\"name\":\"wr_vid\",\"value\":\"9\"}]"
	cs := newCookieStore(raw)
	if cs.vals["wr_vid"] != "9" {
		t.Fatalf("BOM 导致解析失败: %+v", cs.vals)
	}
}

func TestCookieStoreTruncatedJSON(t *testing.T) {
	// 粘贴时漏掉结尾的 ] —— 最常见的失败
	raw := `[{"domain":".weread.qq.com","name":"wr_vid","value":"11"},
	         {"domain":".weread.qq.com","name":"wr_skey","value":"bb"},`
	cs := newCookieStore(raw)
	if cs.vals["wr_vid"] != "11" || cs.vals["wr_skey"] != "bb" {
		t.Fatalf("截断修复失败: %+v", cs.vals)
	}
}

func TestCookieStoreEmptyReportsZero(t *testing.T) {
	cs := newCookieStore(`[{"domain":".qq.com","name":"pgv_pvid","value":"1"}]`)
	if len(cs.vals) != 0 {
		t.Fatalf("只有其他站点 Cookie 时应为 0 条，得到 %d", len(cs.vals))
	}
}

func TestShardError(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "rate" / "api" / "none"
	}{
		{"限流", `{"errcode":-10102,"errmsg":"Hit frequency control"}`, "rate"},
		{"鉴权失败", `{"errCode":-2010,"errMsg":"用户不存在"}`, "api"},
		{"TXT 探测响应", `{"bookId":"123","format":"txt"}`, "none"},
		{"正常分片", `C860C248EB116AE459D0572E52FAEED3PPD94bWw0`, "none"},
		{"不是 JSON", `abc`, "none"},
	}
	for _, c := range cases {
		err := shardError(c.in)
		got := "none"
		var rl *rateLimitError
		if err != nil {
			got = "api"
			if errors.As(err, &rl) {
				got = "rate"
			}
		}
		if got != c.want {
			t.Fatalf("%s: 期望 %s，得到 %s（err=%v）", c.name, c.want, got, err)
		}
	}
}
