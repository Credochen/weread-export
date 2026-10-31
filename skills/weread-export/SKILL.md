---
name: weread-export
description: 把微信读书的书导出成本地 EPUB。适用于"下载微信读书的书/导出微信读书/把书弄到本地/转成 epub/离线看微信读书"，以及调试导出失败（-2010、-10102、分片 MD5 不匹配、限流、章节抓不全）的场景。本机已有现成的 Go 网页版工具，优先复用，不要从零重写。
agent_created: true
---

# 微信读书导出 EPUB

## 结论先行

代码就在本仓库（`skills/` 的上一级）。换机器 clone 后先构建：`cd go && go build -buildvcs=false -o ../weread-export .`

本机已经有成套工具，**直接用，别重写**：

```
~/Documents/weread-export/
├── 开始使用.command      双击启动（开终端跑服务 + 自动开浏览器）
├── weread-export         Go 二进制，9.8MB，前端已内嵌
├── go/                   源码（可改）
├── fetch_weread_epub.py  上游 Python 脚本（命令行批量用）
├── probe.py              链路体检（只报状态，不打印正文）
└── out/                  导出的 EPUB
```

技术来源：[finlater/weread.koplugin](https://github.com/finlater/weread.koplugin) 的 `scripts/fetch_weread_epub.py`。
插件那 220 个 Lua 文件**不用管**，90% 是 KOReader 的 UI。

## 使用流程

1. 双击 `开始使用.command`（或 `./weread-export -port 8791 -out out`）
2. 浏览器打开 `http://127.0.0.1:8791`
3. 填三样：
   - **阅读页链接**：`https://weread.qq.com/web/reader/<hash>`
   - **Cookie**：Cookie-Editor 插件导出的整份 JSON，直接粘（也认 `wr_vid=...; wr_skey=...`）
   - 点「检测连接」→ 选范围 → 导出
4. 完成后下载，文件同时落在 `out/`

范围支持 `1-20,35,60-` 这种写法；**断点续抓**就用它，比如 `29-`。

## 算法三层（改代码时才需要）

都在 `go/weread_core.go`，有交叉验证测试，动之前先跑 `go test ./...`：

| 函数 | 作用 |
|---|---|
| `WereadE` | bookId / chapterUid / 时间戳 → 内部 ID。阅读页 URL 里的 hash 就是它 |
| `WereadSign` | 排序 query 后从后往前成对位移异或，结果作为参数 `s` |
| `DecodeChapter` | `e_0+e_1+e_3` → MD5 校验 → 换位还原 → base64 → UTF-8 重组 |

请求链：`renewal` → 阅读页扒 `__INITIAL_STATE__` 拿 `psvts` → `chapterInfos` 拿目录 → 逐章拉 `e_0/e_1/e_3`（正文）、`e_2`（样式，`st=1`）、`chapter.tar`（图片）。

TXT 格式的书：`e_0` 会返回含 `"bookId"` 的 JSON，此时降级用 `t_0/t_1`。

## 坑清单（全是真踩过的）

### 认证类

| 现象 | 根因 | 处理 |
|---|---|---|
| `-2010 用户不存在` 但 Cookie 是好的 | `renewal` 鉴权失败会回 `Set-Cookie` 把 `wr_skey` 置空，早期版本照抄写回本地，自己把登录态删了 | 续期失败**不提交** Set-Cookie，空值永不覆盖 |
| `-2013 鉴权失败` | renewal 对 Web Cookie 本来就会失败 | 忽略即可，不影响 `chapterInfos` |
| 登录判定错误 | 阅读页 `__INITIAL_STATE__.user.name` 常常是空的 | 以 `chapterInfos` 是否成功为判据；账号名从 `wr_name` Cookie 解 |

**HttpOnly 是硬门槛**：`wr_vid` / `wr_skey` / `wr_rt` 全是 HttpOnly，页面 JS 读不到，`document.cookie` 永远空。

### 抓取类

| 现象 | 根因 | 处理 |
|---|---|---|
| `分片 MD5 不匹配: expected={"errcode":-10102...}` | **被限流**。服务端回 JSON 错误体，被当正文送去解码 | 识别为限流；4/8/16/32/60 秒退避重试 5 次，请求间隔翻倍（上限 3s） |
| 长书跑到一半断 | 每章 4~6 个请求太密 | 单章失败跳过继续，最后汇总报告；建议分批导（每本间隔几分钟） |
| 内容接口返回 `{}` | 签名错 / `false` 拼成 `False` / `ps` 过期 / 无该章权益 | `ps` 必须每章重新从阅读页取 |
| 图片空白 | XHTML 还指向远程鉴权 URL | 下载 `chapter.tar`，把 `src` 改写成 `../images/xxx` |

### Go 实现类

| 坑 | 说明 |
|---|---|
| RE2 不支持反向引用 | `src=(['"])(.*?)\1` 直接 panic，必须手写扫描配对引号 |
| `mimetype` 不能设 `Modified` | 否则 Go 会写扩展时间戳 extra 字段，严格阅读器拒收；且必须首位 + `zip.Store` |
| `0x15051505` 超 int32 | 用 `int64` 算再 `& 0x7FFFFFFF` |
| SSE 字段 `omitempty` | `done:0` 会被整个吞掉，前端算出 NaN |
| 目录在 git 仓库里 | `go build` 报 `error obtaining VCS status`，加 `-buildvcs=false` |

### 环境类（macOS）

- **钥匙串授权框弹不出来**：后台进程不在图形会话里。想读 Edge 的 Cookie 库（`Microsoft Edge Safe Storage` + PBKDF2 1003 轮 + AES-128-CBC）必然卡在这里。这条路已放弃，改手工粘贴。
- **CDP 也走不通**：复制 Edge 配置加 `--remote-debugging-port`，Cookie 解不开，`Network.getCookies` 返回空。而且 `Storage.getCookies` 会挂住；`Network.enable` 在 `edge://` 页面上不响应，得用真实页面 target。
- **`open xxx.command` 在工具会话里调不起 GUI**，让用户自己双击。

## 自检

```bash
cd ~/Documents/weread-export/go
go vet ./... && go test ./...     # 19 个用例
./weread-export selftest          # 算法自检（3 组官方向量 + appId + 签名 + 换位）
python3 ../probe.py --reader-url "https://weread.qq.com/web/reader/<hash>" \
  --cookie-string "<cookie>"      # 分层链路体检，不打印正文
```

## 合规

只导**用户自己有阅读权益**的书，个人离线阅读；不分发、不商用、不传公开仓库。
Cookie 属于凭证：用完即弃，不要写进任何会提交的文件；用户贴过的凭据要提醒其重新登录使其失效。
