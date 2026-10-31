#!/usr/bin/env python3
"""
微信读书导出链路体检。

分层跑一遍整条管道，只报状态，不打印任何正文。
用法：
    python3 probe.py --reader-url "https://weread.qq.com/web/reader/xxxx" \
                     --cookie-string "wr_vid=...; wr_skey=...; wr_rt=..."

退出码：0 全通 / 2 有环节失败 / 1 致命错误
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import re
import sys
import unicodedata
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent


def load_impl():
    """把 fetch_weread_epub.py 当模块加载（dataclass 需要先进 sys.modules）"""
    path = HERE / "fetch_weread_epub.py"
    spec = importlib.util.spec_from_file_location("weread_impl", path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["weread_impl"] = mod
    spec.loader.exec_module(mod)
    return mod


PASS, FAIL, SKIP = "✓", "✗", "－"


def normalize_cookie(raw: str) -> str:
    """插件导出的 JSON 数组 → 原始 Cookie 头；其他站点条目丢弃。"""
    text = (raw or "").strip()
    if not text.startswith(("[", "{")):
        return text
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        return text
    if isinstance(data, dict):
        data = data.get("cookies", [])
    pairs = []
    for item in data if isinstance(data, list) else []:
        name = item.get("name")
        domain = item.get("domain", "")
        if not name or "weread.qq.com" not in domain:
            continue
        pairs.append(f"{name}={item.get('value', '')}")
    return "; ".join(pairs) if pairs else text


def disp_width(text: str) -> int:
    """终端显示宽度：CJK 算 2 列"""
    return sum(2 if unicodedata.east_asian_width(ch) in "WF" else 1 for ch in text)


def pad(text: str, width: int) -> str:
    return text + " " * max(0, width - disp_width(text))


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--reader-url", required=True, help="阅读页完整 URL")
    ap.add_argument("--cookie-string", help="浏览器里复制的 Cookie 整行")
    ap.add_argument("--cookie-file", type=Path, help="Netscape 格式 cookie 文件")
    ap.add_argument("--chapter-limit", type=int, default=1, help="体检时抓几章，默认 1")
    args = ap.parse_args(argv)

    impl = load_impl()
    rows: list[tuple[str, str, str]] = []
    state = None

    def add(name: str, mark: str, detail: str) -> None:
        rows.append((name, mark, detail))
        print(f"  {mark}  {pad(name, 24)} {detail}", flush=True)

    print("\n微信读书导出链路体检")
    print("=" * 72)

    # 1. 网络连通
    try:
        req = urllib.request.Request(
            "https://weread.qq.com/",
            headers={"user-agent": impl.USER_AGENT, "accept": "text/html,*/*"},
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            add("1 网络连通", PASS, f"HTTP {resp.status}")
    except Exception as exc:
        add("1 网络连通", FAIL, f"{type(exc).__name__}: {exc}")
        print("\n网络不通，后面不用测了。检查代理 / 防火墙。")
        return 1

    cookie_string = normalize_cookie(args.cookie_string or "")
    has_cookie = bool(cookie_string or args.cookie_file)
    client = impl.WeReadClient(
        cookie_file=args.cookie_file,
        cookie_string=cookie_string,
        save_cookies=None,
    )

    # 2. 登录态
    if has_cookie:
        try:
            ok = client.renew()
            add("2 登录态续期", PASS if ok else FAIL, "renewal succ" if ok else "renewal 未返回 succ")
        except Exception as exc:
            add("2 登录态续期", FAIL, f"{type(exc).__name__}: {str(exc)[:80]}")
    else:
        add("2 登录态续期", SKIP, "未提供 Cookie，跳过（后续步骤大概率失败）")

    # 3. 阅读页解析
    try:
        html = client.get_text(args.reader_url, referer=args.reader_url)
        state = impl.read_reader_state(html)
        user = (state.initial_state.get("user") or {})
        name = user.get("name") or ""
        detail = f"《{state.book_title}》{state.author} / bookId={state.book_id}"
        add("3 阅读页解析", PASS, detail)
        if name:
            add("3b 账号识别", PASS, f"已登录：{name}")
        else:
            add("3b 账号识别", FAIL, "未识别到登录用户（游客态）")
        add("3c psvts", PASS if state.psvts else FAIL, (state.psvts or "<空>")[:28])
        # 自检：正向编码是否与 URL 里的 hash 一致
        m = re.search(r"/web/reader/([0-9a-z]+)", args.reader_url)
        if m:
            calc = impl.weread_e(state.book_id)
            add("3d 编码器自检", PASS if calc == m.group(1) else FAIL,
                "e(bookId) 与 URL hash 一致" if calc == m.group(1)
                else f"算出 {calc}，URL 是 {m.group(1)}")
    except Exception as exc:
        add("3 阅读页解析", FAIL, f"{type(exc).__name__}: {str(exc)[:90]}")

    if state is None:
        return 1

    # 4. 章节目录（登录态的硬门槛）
    chapters = []
    try:
        payload = client.post_json(
            "https://weread.qq.com/web/book/chapterInfos",
            {"bookIds": [state.book_id]},
            referer=args.reader_url,
        )
        if isinstance(payload, dict) and payload.get("errCode"):
            add("4 章节目录", FAIL, f"errCode={payload['errCode']} {payload.get('errMsg','')}")
        else:
            book_record, chapters = impl.normalize_chapter_infos(payload, state.book_id)
            chapters = [c for c in chapters
                        if int(c.get("wordCount") or 0) > 0
                        and str(c.get("title") or "") != "封面"]
            fmt = book_record.get("format") or "?"
            add("4 章节目录", PASS, f"{len(chapters)} 章，格式={fmt}")
    except Exception as exc:
        add("4 章节目录", FAIL, f"{type(exc).__name__}: {str(exc)[:90]}")

    # 5. 分片 + 解码
    if chapters:
        ch = chapters[0]
        try:
            ref = impl.reader_url_for(state.book_id, ch["chapterUid"])
            psvts = impl.read_reader_state(client.get_text(ref, referer=ref)).psvts

            def shard(ep: str, style: bool = False) -> str:
                params = impl.make_content_params(state.book_id, ch["chapterUid"], psvts,
                                                  style=style, sc=1)
                return client.request("https://weread.qq.com" + ep, method="POST",
                                      data=params, referer=ref).decode("utf-8", "replace")

            e0 = shard("/web/book/chapter/e_0")
            if e0.startswith("{") and '"bookId"' in e0:
                t0 = shard("/web/book/chapter/t_0")
                plain = impl.decode_content_shards(t0, shard("/web/book/chapter/t_1"), "")
                add("5 正文分片", PASS, f"TXT 格式，解码 {len(plain)} 字符")
            else:
                e1, e3 = shard("/web/book/chapter/e_1"), shard("/web/book/chapter/e_3")
                add("5a 分片抓取", PASS, f"e0/e1/e3 = {len(e0)}/{len(e1)}/{len(e3)} 字节")
                content = impl.decode_content_shards(e0, e1, e3)
                paras = content.count("<p")
                add("5b 分片解码", PASS,
                    f"MD5 校验通过，{len(content)} 字符 / {paras} 个段落")
                if paras < 3:
                    add("5c 正文密度", FAIL, "段落过少，可能是试读页或该章无权益")
                else:
                    add("5c 正文密度", PASS, "正文正常")
        except Exception as exc:
            add("5 正文分片", FAIL, f"{type(exc).__name__}: {str(exc)[:90]}")
    else:
        add("5 正文分片", SKIP, "无可用章节")

    # 结论
    print("=" * 72)
    fails = [r for r in rows if r[1] == FAIL]
    hard = [r for r in fails if r[0].startswith(("1", "3 ", "4", "5"))]
    if not fails:
        print("结论：全链路通。可以直接跑 fetch_weread_epub.py 导整本。")
        print(f"\n  python3 fetch_weread_epub.py --reader-url \"{args.reader_url}\" \\")
        print("    --cookie-file ./cookies.txt --output ./out/book.epub")
        return 0
    if not hard:
        print("结论：主链路通，有非致命问题（见上）。可以先试跑 2 章。")
        return 0
    print("结论：存在阻断项：")
    for name, _, detail in hard:
        print(f"  · {name}: {detail}")
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
