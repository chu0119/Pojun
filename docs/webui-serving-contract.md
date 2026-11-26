# 前端静态产物的 HTTP 契约

内嵌前端（`server/webui/dist`，由 `web/out` 复制而来）由
`server/webui_handler.go` 提供。本文件是该 handler 的行为契约：改它之前先看
这里，改完同步更新这里与 `server/webui_handler_test.go`。

## 1. 命名空间

一个请求路径只属于一个命名空间，判定集中在 `classifyWebuiPath`：

| 路径 | 命名空间 | 行为 |
|---|---|---|
| `/`、`/login/`、`/system/agents/chain`（无扩展名） | 文档 | 命中导出的 HTML；未命中回 404 页 |
| `*.html` | 文档 | 精确命中；不做 301（避免 `/x/index.html` → `./x/`） |
| `/_next/*`、`/__next.*` | 资产 | 必须精确命中 |
| 其它带扩展名的路径（`.js` `.css` `.txt` `.ico` `.woff2` `.map` …） | 资产 | 必须精确命中 |
| `/_pojun_build.txt` | 身份 | 虚拟文件，见 §3 |

**为什么**：文档可以"回落到一个页面"，资产不能。历史上任何未命中路径（包括
`_next/static` 下已不存在的 chunk、`dashboard/index.txt` 这类 RSC 载荷）都被
回成 200 的根 HTML，导致 404 语义丢失、`text/javascript` 变成 `text/html`，
并可能被浏览器缓存一年。

## 2. 缓存与状态码

| 情形 | 状态 | Cache-Control | 其它 |
|---|---|---|---|
| `_next/static/**` 真实文件 | 200 | `public, max-age=31536000, immutable` | `ETag`；Next 对文件名内容寻址 |
| 其它资产命中 | 200 | `no-cache` | `ETag`，支持 Range/304 |
| 资产未命中 | **404** | `no-store` | `text/plain`，**绝不回 HTML** |
| 文档命中 | 200 | `no-cache` | `Vary: Accept`、`ETag`、注入 build meta |
| 文档未命中 | **404** | `no-store` | 返回导出的 `404.html` 本体 |
| `/_pojun_build.txt` | 200 | `no-cache` | `text/plain`，`ETag`，`If-None-Match` → 304 |
| 非 GET/HEAD | **405** | `no-store` | `Allow: GET, HEAD` |

所有响应都带 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、
`Referrer-Policy: no-referrer`（此前只有 `/api` 有，文档却声称全站生效）。

Content-Type 由扩展名表显式给出（`text/javascript`、`text/css`、`font/woff2`
等），不走 `mime.TypeByExtension`：Windows 会读注册表，同一个二进制在不同宿主
上会给 `.js` 不同的 MIME。

## 3. 构建身份

- 来源：对内嵌 `dist` 的全量文件按 `relpath \0 content \0`（路径排序）做
  sha256，取前 16 位十六进制。**同一份产物恒等，改一个字节即变**。
- 暴露面：
  - 每个导出的 HTML 在 `<head>` 之后注入
    `<meta name="pojun-build" content="<16 hex>">`；
  - `GET /_pojun_build.txt` → 纯文本 16 位（无产品名，未鉴权可读，用于验收与
    存活页面自检）；
  - `GET /api/version`（鉴权）→ `{"service","version","web_build"}`。
- 客户端只接受 `^[0-9a-f]{16}$`；不合法视为"无法判定"并停用所有基于代次的
  自动行为（防止旧服务端把 HTML 当 build.txt 返回时造成永久误报）。

## 4. 与前端的关系

`web/src/lib/build-policy.ts` 与两个 guard（`components/runtime/`）按本契约工作：
页面启动时读 meta 作为自身代次，在 mount / bfcache `pageshow` / `focus` /
可见时每 60s 取 `/_pojun_build.txt` 比对。只对"链页 + 无未保存草稿 + 页面刚
打开 15s 内"自动重载一次（30s 冷却 + 抖动），其余情况出常驻横幅由用户点。

## 5. 行为变化（升级须知）

| 路径 | 旧行为 | 新行为 |
|---|---|---|
| `/_next/static/chunks/<不存在>.js` | 200 `text/html` + 一年 immutable | 404 `text/plain` + `no-store` |
| 任意未知深链（`/no-such-route/`） | 200 首页 | 404 页（`404.html` 本体，状态码 404） |
| `dashboard/index.txt` 等 RSC 载荷缺失 | 200 HTML | 404 `text/plain` |
| `GET /api`（精确） | 301 → `/api/` | 直接进 API（401/404） |
| `/404/`、`/_not-found/` | 200 | 200（导出的页面本体仍在，与 Next 行为一致） |

### 回退开关

`POJUN_WEBUI_SHELL_FALLBACK=1`：**只**让文档命名空间的未命中回落到外壳
（200 + `no-cache`），资产命名空间保持 404。用于严格 404 引起未预期深链问题时
应急，保留一个发布周期后删除。它不会把"缺失 chunk 回 200 HTML"带回来。

## 6. 验收矩阵

```bash
B=localhost:8787
curl -sI  $B/_next/static/chunks/nonexistent.js   # 404, text/plain, no-store, 无 immutable
curl -sI  $B/_next/static/chunks/<当前存在的>.js    # 200, text/javascript, immutable
curl -sI  $B/                                     # 200, text/html, no-cache, 含 meta pojun-build
curl -sI  $B/no-such-route/                       # 404, text/html(404 页), no-store
curl -sI  $B/api                                  # 401（不再是 301/200 HTML）
curl -s   $B/_pojun_build.txt                     # 16 位 hex；带 If-None-Match → 304
curl -s   -H "Authorization: Bearer <token>" $B/api/version   # version + web_build 一致
```

`scripts/acceptance-webui.sh`（在 pojun skill 的 scripts 目录）把上面这套封装成
一条命令，任一不符即非零退出。
