# fs-hash-util — 详细实现计划

> 本文档为实现计划（design & plan），**不含实现代码**。据 `README.md` 需求编写。
> 生成日期：2026-09-20 · 最后修订：2026-09-20（依据 review 更新）

---

## 1. 项目目标

用 Go 实现一个**本地文件哈希工具**：扫描指定目录（含子目录）下所有文件，计算 SHA-256 哈希，把结果存入**文件型数据库**，供后续查询与维护。重扫描时对未变更文件跳过重算。

### 交付物

- 单一可执行二进制 `fshash`，交叉编译支持：
  - `darwin/arm64`（macOS Apple Silicon）
  - `linux/amd64`（ubuntu x64）
  - `linux/arm64`（ubuntu arm64）

---

## 2. 关键技术决策（建议方案，已设计确认）

| 决策项 | 选定方案 | 理由 |
|--------|----------|------|
| DB 引擎 | **SQLite**（`modernc.org/sqlite`，纯 Go / CGO-free） | 单文件、支持 SQL 查询与索引、事务、增量 upsert；纯 Go 驱动可无痛交叉编译到三平台 |
| CLI 框架 | **`spf13/cobra`** 子命令式 | `fshash scan / query / list / stats`，扩展性好 |
| 排除规则 | **内置默认 + `--exclude` regex flag** | 开箱即用（默认排 `node_modules`、`.git` 等）又可自定义 |
| 哈希算法 | **SHA-256**（`crypto/sha256`，流式读取） | README 指定；空文件用真实空串 SHA-256 |
| 并发 | worker pool（`errgroup` + 有界并发） | 大目录下加速哈希计算 |
| SQLite 写入模型 | **单 writer goroutine** + `SetMaxOpenConns(1)` | 避免 per-connection PRAGMA 失效；WAL 模式下并发读不受影响 |
| 扫描代次标识 | **自增 `scan_id`**（`scans` 表） | 用时间戳作代次在同秒内多次扫描时会冲突；自增 ID 无此问题 |

> ⚠️ **CGO-free 硬约束**：必须使用 `modernc.org/sqlite`（纯 Go），**不可**用 `mattn/go-sqlite3`（需 CGO，交叉编译困难）。构建时 `CGO_ENABLED=0`。

---

## 3. 目录结构

```
fs-hash-util/
├── README.md
├── go.mod
├── go.sum
├── main.go                     # 入口，调用 cmd.Execute()
├── Makefile                    # 交叉编译 / test / lint 目标
├── docs/
│   └── IMPLEMENTATION_PLAN.md  # 本文档
├── cmd/                        # cobra 命令定义
│   ├── root.go                 # 根命令 + 全局 flag (--db, --verbose)
│   ├── scan.go                 # scan 子命令
│   ├── query.go                # query 子命令
│   ├── list.go                 # list 子命令
│   └── stats.go                # stats 子命令
└── internal/
    ├── db/
    │   ├── db.go               # 打开/初始化 DB、schema migration
    │   ├── model.go            # FileRecord / ScanRecord 结构体
    │   └── repository.go       # CRUD：Upsert / Get / List / Query / Exists
    ├── scanner/
    │   ├── scanner.go          # 目录遍历 + 排除规则 + 增量判断
    │   └── exclude.go          # 默认排除模式 + regex 匹配
    └── hasher/
        └── hasher.go           # SHA-256 流式哈希，空文件真实 SHA-256
```

> `internal/` 保证包不被外部 import，保持内聚。

---

## 4. 数据模型与 DB Schema

### 4.1 FileRecord（`internal/db/model.go`）

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `ID` | `int64` | 自增主键 |
| `FileName` | `string` | 文件名（basename） |
| `RelPath` | `string` | 相对 scan root 的相对路径（含文件名，使用 `/` 分隔符统一跨平台） |
| `BirthTS` | `*int64` | 文件创建时间（Unix nanoseconds）；平台不支持时为 `NULL`（见 §7） |
| `ModifiedNS` | `int64` | 文件最后修改时间戳（Unix **nanoseconds**，来自 `fi.ModTime().UnixNano()`） |
| `Size` | `int64` | 文件字节数 |
| `Hash` | `string` | SHA-256 十六进制；`Size==0` 时用空串的真实 SHA-256 |
| `ScanRoot` | `string` | 本条记录所属的扫描根（经 `filepath.EvalSymlinks` + `filepath.Clean` 规范化的绝对路径） |
| `ScanID` | `int64` | 关联 `scans.id`，标识最近一次确认该文件存在的扫描 |

> **时间精度升级**：字段从 Unix 秒改为 Unix **nanoseconds**（`UnixNano()`）。`ModifiedNS` 替代原 `ModifiedTS`，避免同尺寸文件在一秒内被修改时错误跳过哈希。

### 4.2 ScanRecord（`internal/db/model.go`）

| 字段 | Go 类型 | 说明 |
|------|---------|------|
| `ID` | `int64` | 自增主键，作为扫描代次标识 |
| `ScanRoot` | `string` | 本次扫描的根路径 |
| `StartedAt` | `int64` | 扫描开始时间（Unix nanoseconds，仅作记录，不作代次判断） |
| `FinishedAt` | `*int64` | 扫描完成时间；`NULL` 表示尚未完成或异常中断 |

### 4.3 SQL Schema（`scan` 首次运行时建表）

```sql
CREATE TABLE IF NOT EXISTS scans (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_root   TEXT    NOT NULL,
    started_at  INTEGER NOT NULL,
    finished_at INTEGER             -- NULL = in-progress or failed
);

CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_root    TEXT    NOT NULL,
    file_name    TEXT    NOT NULL,
    rel_path     TEXT    NOT NULL,
    birth_ts     INTEGER,           -- NULL when platform cannot provide birth time
    modified_ns  INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    hash         TEXT    NOT NULL,
    scan_id      INTEGER NOT NULL REFERENCES scans(id),
    UNIQUE(scan_root, rel_path)
);

CREATE INDEX IF NOT EXISTS idx_files_hash     ON files(hash);
CREATE INDEX IF NOT EXISTS idx_files_relpath  ON files(rel_path);
CREATE INDEX IF NOT EXISTS idx_files_scan_id  ON files(scan_id);

-- schema 版本管理
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- 初始化写入 schema_version=1
```

**增量跳过键**：README 要求"same relative-path + file-name + file-size + last-update-time 视为未变更"。
- `rel_path` 已含 file-name，故唯一约束用 `(scan_root, rel_path)`。
- 跳过判定：查现有记录，若 `size` 与 `modified_ns` 均相同 → 跳过哈希计算，仅更新 `scan_id`。

**DB 连接配置**（`db.Open` 中设置）：
```go
db.SetMaxOpenConns(1)   // 单连接，保证 per-connection PRAGMA 生效
db.SetMaxIdleConns(1)
```
```sql
PRAGMA journal_mode=WAL;     -- 并发读写更稳
PRAGMA synchronous=NORMAL;   -- 性能与安全折中
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
```
> PRAGMA 通过 connection hook 或 DSN 参数（`?_journal_mode=WAL&_busy_timeout=5000`）在每次连接建立时设置，而非一次性执行。

---

## 5. 模块设计

### 5.1 `internal/hasher`

- `HashFile(path string) (string, error)`：`os.Open` → `io.Copy` 到 `sha256.New()` → 返回 hex。
- **空文件**（`size == 0`）：返回固定常量
  `const EmptyFileHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`
  （空串的真实 SHA-256，语义正确且可与非空文件统一比较）。
- 流式读取，避免大文件一次性载入内存。

### 5.2 `internal/scanner`

**排除规则（`exclude.go`）**：

完整默认排除列表（`DefaultExcludes`），匹配对象为 slash-normalized 相对路径，**区分大小写**，任一命中即跳过：

```go
var DefaultExcludes = []string{
    `(^|/)\.git(/|$)`,
    `(^|/)node_modules(/|$)`,
    `(^|/)\.svn(/|$)`,
    `(^|/)\.hg(/|$)`,
    `(^|/)\.DS_Store$`,
    `(^|/)Thumbs\.db$`,
    `(^|/)__pycache__(/|$)`,
    `(^|/)\.venv(/|$)`,
    `(^|/)target(/|$)`,        // Rust/Maven build output
    `(^|/)dist(/|$)`,
    `(^|/)\.idea(/|$)`,
    `(^|/)\.vscode(/|$)`,
}
```

规则说明：
- 用户 `--exclude` 规则**追加**到默认规则（逻辑 OR），不覆盖。
- `--no-default-excludes` 仅保留用户规则。
- 无效 regex 在启动时即 `fmt.Fprintf(os.Stderr, ...) + os.Exit(1)`，不进入扫描。
- `Matcher.ShouldSkip(relPath string, isDir bool) bool`：命中目录时整个子树剪枝。

**DB 文件自动排除**：

`scan` 命令在构建 `Matcher` 时，将 DB 文件的 canonical absolute path 加入排除列表，同时排除其 WAL 和 SHM 伴生文件：

```go
// 排除 DB 自身及 SQLite 伴生文件，防止自扫描
dbAbs, _ := filepath.Abs(dbPath)
dbAbs = filepath.Clean(dbAbs)
excludeAbsPaths = []string{dbAbs, dbAbs + "-wal", dbAbs + "-shm"}
```

扫描时对每个文件调用 `os.Lstat`（不跟随 symlink），再用 canonical absolute path 比对 `excludeAbsPaths`，命中则跳过，**不依赖 regex 匹配文件名**。

**扫描根路径规范化**：

```go
root, err := filepath.EvalSymlinks(dir)  // 解析 symlink
root = filepath.Clean(filepath.Abs(root))
```

`filepath.EvalSymlinks` 确保通过不同路径（symlink、`..`、大小写别名）访问同一目录时，`scan_root` 在 DB 中唯一。若平台区分大小写（Linux），则直接使用；macOS 大小写不敏感，以 `EvalSymlinks` 返回值为准。

**文件类型过滤**：

只处理 `DirEntry.Type().IsRegular()` 的普通文件：
- symlink（指向文件或目录）：**跳过，不记录**（v1）；文档注明。
- socket、FIFO、device file：跳过。
- `os.Lstat` 替代 `os.Stat` 取 metadata，避免意外跟随 symlink。

**scanner.go**：

- `Scan(ctx context.Context, root string, opts ScanOpts) (ScanResult, error)`。
- 用 `filepath.WalkDir`。
- 相对路径规范化：`filepath.Rel(root, path)` 后 `filepath.ToSlash()`。

### 5.3 `internal/db`

- `Open(path string) (*DB, error)`：`sql.Open("sqlite", dsn)` + `SetMaxOpenConns(1)` + PRAGMA + `migrate()`。
  - DSN 格式：`file:<path>?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_foreign_keys=ON`
- `repository.go`：
  - `CreateScan(scanRoot string, startedAt int64) (int64, error)` — 插入 `scans`，返回 `scan_id`。
  - `FinishScan(scanID int64, finishedAt int64) error` — 更新 `finished_at`；只有调用此函数后 prune 才合法。
  - `GetByRelPath(scanRoot, relPath string) (*FileRecord, bool, error)` — 增量判断用。
  - `Upsert(rec FileRecord) error` — `INSERT ... ON CONFLICT(scan_root, rel_path) DO UPDATE`。
  - `List(scanRoot string, limit, offset int) ([]FileRecord, error)`。
  - `QueryByHash(hash string) ([]FileRecord, error)` — 精确哈希匹配。
  - `DeleteNotSeen(scanRoot string, scanID int64) (int64, error)` — 删除 `scan_id < scanID` 的同 root 记录；**仅在 `FinishScan` 之后调用**。
  - 批量写用**事务**（每 500 条 commit 一次）提升吞吐；单 writer goroutine 负责所有写入。

### 5.4 `cmd`（cobra）

**全局 flag（root.go）**：`--db <path>`（默认 `./fshash.db`）、`--verbose`。

| 子命令 | 用法 | 行为 |
|--------|------|------|
| `scan` | `fshash scan <dir> [--db f] [--exclude re]... [--no-default-excludes] [--concurrency N] [--prune]` | 扫描目录写入/更新 DB；`--prune` 仅在完整扫描成功后执行 |
| `query` | `fshash query --hash <sha256>` 或 `--path <relpath>` | 按精确哈希或精确相对路径（`/`-normalized）查询并打印 |
| `list` | `fshash list [--root <dir>] [--limit N] [--offset N]` | 列出记录（表格输出，按 rel_path 字母序排序） |
| `stats` | `fshash stats [--root <dir>]` | 打印文件总数、总大小、重复哈希数等汇总 |

**CLI contract**：

| 场景 | exit code | 输出目标 |
|------|-----------|---------|
| 正常完成 | `0` | summary → stdout |
| 部分文件读取失败（扫描继续） | `0`（错误计入 summary） | 每条错误 → stderr |
| 致命错误（DB 无法打开、无效参数等） | `1` | 错误信息 → stderr |
| 遍历中断（context cancel、权限拒绝导致根目录无法读取） | `1` | 错误信息 → stderr；**不执行 prune** |

路径查询（`query --path`）接受 `/`-normalized 相对路径（与 DB 存储格式一致）；跨平台用户若在 Windows 使用，需自行转换分隔符（v1 不做自动转换）。

---

## 6. scan 主流程（伪逻辑）

```
scan(dir):
  root := evalSymlinks(clean(abs(dir)))
  dbAbs := clean(abs(dbPath))
  db := open(dbPath); db.migrate()
  matcher := buildExcludeMatcher(defaults, userExcludes, dbAbsExcludeList)

  scanID := db.CreateScan(root, now())   // 建立扫描记录，取自增 ID
  traversalOK := false

  jobs    := make(chan Job, concurrency*2)   // 有界
  results := make(chan Result, concurrency*2) // 有界

  // goroutine A: WalkDir → jobs
  go func():
    defer close(jobs)
    err := WalkDir(root, func(path, entry):
      rel := toSlash(rel(root, path))
      absPath := abs(path)

      if absPath in dbAbsExcludeList: continue       // 排除 DB 及伴生文件
      if matcher.ShouldSkip(rel, isDir):
          if isDir: return SkipDir
          else: continue
      if !entry.Type().IsRegular(): continue         // 只处理普通文件
      stat := lstat(path)                            // os.Lstat，不跟随 symlink
      existing, found := db.GetByRelPath(root, rel)
      if found && existing.size==stat.size && existing.modified_ns==stat.mtime.UnixNano():
          jobs <- TouchJob{id: existing.id, scanID: scanID}   // 仅更新 scan_id
          continue
      jobs <- HashJob{path, rel, stat, scanID}
    )
    if err != nil:
      cancelCtx()    // 通知 workers 和 collector 中止
      traversalError = err

  // goroutine B: jobs → workers → results
  go func():
    defer close(results)
    errgroup 并发 N workers:
      for job in jobs:
        if TouchJob: results <- TouchResult{...}; continue
        hash := hasher.HashFile(job.path)
        results <- HashResult{job, hash}

  // goroutine C (main goroutine): results → DB（单 writer）
  for result in results:
    db.Upsert / db.TouchScanID(...)  // 批量事务，每 500 条 commit

  wait A, B goroutines

  if traversalOK && traversalError == nil && contextNotCancelled():
    db.FinishScan(scanID, now())
    if --prune:
      db.DeleteNotSeen(root, scanID)   // 只删 scan_id < scanID 的同 root 记录
  else:
    // 遍历未完成：记录错误，不 prune，不 FinishScan，退出码 1
    return error

  print summary (total / hashed / skipped / errors)
```

**关键并发约定**：
- goroutine A（WalkDir）、goroutine B（worker pool）、goroutine C（DB writer）**三者同时运行**，通过有界 channel 背压协调。
- `jobs` channel 由 goroutine A 关闭；`results` channel 由 goroutine B（errgroup 完成后）关闭。
- 所有 DB 写入仅由 goroutine C（main goroutine）执行——单 writer 保证事务隔离。
- context cancel 时 worker 停止读 jobs，goroutine A 的 WalkDir 通过 ctx 传播取消。
- `--prune` **必须且仅在** `FinishScan` 调用后执行；任何 traversal error、worker error 或 context 取消都禁止执行 prune。

---

## 7. 平台差异与注意点

- **创建时间（BirthTS）**：Go 标准库无跨平台创建时间 API。
  - 方案：通过 `os.Lstat` 的 `Sys()` 断言到 `*syscall.Stat_t`，**Linux**：`Ctim` 是 inode change time，**不是**文件创建时间；如无 `Btime` 字段（部分内核版本支持），保存 `NULL`。**macOS（darwin）**：`Birthtimespec` 为真正的 birth time，可用。
  - 用**构建标签分文件**：`stat_darwin.go` / `stat_linux.go` 各实现 `birthTimeNS(fi os.FileInfo) *int64`；无法取得时返回 `nil`（存为 `NULL`）。
  - **不**将 ctime / mtime 无标记地当作创建时间存入 `birth_ts`；`NULL` 比错误数据更诚实。
- **修改时间**：`fi.ModTime().UnixNano()` 跨平台可靠，直接用；精度为 nanoseconds（`modified_ns`）。
- **路径分隔符**：DB 内统一存 `/`（`filepath.ToSlash`），保证不同 OS 生成的 DB 可互查。
- **符号链接**：用 `DirEntry.Type().IsRegular()` 过滤，用 `os.Lstat` 取 metadata，不跟随 symlink（v1）；文档注明。socket、FIFO、device file 同样跳过。
- **路径规范化**：`filepath.EvalSymlinks` + `filepath.Clean`，防止 symlink 别名、`..` 路径和大小写（macOS）导致同一目录以多个 `scan_root` 存入 DB。
- **CGO_ENABLED=0**：必须，否则 modernc 优势失效且交叉编译失败。

---

## 8. 交叉编译（Makefile 目标）

```
build-darwin-arm64:  GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/fshash-darwin-arm64
build-linux-amd64:   GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/fshash-linux-amd64
build-linux-arm64:   GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-s -w" -o dist/fshash-linux-arm64
build-all: build-darwin-arm64 build-linux-amd64 build-linux-arm64
```

注入版本号：`-ldflags "-s -w -X main.version=$(git describe --tags --always)"`。

---

## 9. 依赖清单（go.mod）

- `github.com/spf13/cobra` — CLI
- `modernc.org/sqlite` — 纯 Go SQLite 驱动
- `golang.org/x/sync/errgroup` — 并发 worker pool
- （标准库：`crypto/sha256`、`io/fs`、`path/filepath`、`database/sql`、`regexp`、`syscall`）

---

## 10. 测试计划

| 层级 | 内容 |
|------|------|
| 单元 | `hasher`：普通文件 / 空文件返回空串 SHA-256 |
| 单元 | `scanner/exclude`：DefaultExcludes 各条命中；`--no-default-excludes` 仅用户规则；无效 regex 启动时退出；目录命中返回 `SkipDir` |
| 单元 | 路径规范化：`toSlash`、`EvalSymlinks`、DB 文件 canonical path 排除 |
| 单元 | `db`：Upsert 幂等、唯一约束冲突走 update、`QueryByHash`、`DeleteNotSeen` 只删旧 scan_id 记录 |
| 集成 | 临时目录（`t.TempDir()`）造文件树 → scan → 断言 DB 记录；重扫断言跳过计数；改文件后重扫断言重算 |
| 集成 | 排除 `node_modules` 生效（含子树剪枝）；空文件处理；DB 文件本身不被扫描入库 |
| 集成 | `--prune`：完整扫描后删除已删除文件记录；遍历中断时不执行 prune |
| 集成 | `modified_ns` 纳秒精度：同秒内修改的文件能被正确检测为已变更 |
| 边界 | 无权限文件（部分失败，exit 0，summary 含错误计数）；扫描根无权限（fatal，exit 1）；超大文件流式（可选） |

CI（可选）：GitHub Actions matrix 跑三平台 `go build` + `go test`。

---

## 11. 实现阶段（建议顺序）

1. **脚手架**：`go mod init`、`main.go`、cobra root + 空子命令、Makefile。
2. **hasher**：SHA-256 + 空文件处理 + 单测。
3. **db**：Open/migrate（含 `scans` 表）/model/repository + 单测。
4. **scanner**：WalkDir + exclude（含完整 DefaultExcludes + DB 文件排除）+ 增量判断（nanosecond mtime）+ 平台 birth time 文件。
5. **scan 命令**：串起 scanner→hasher→db（含三 goroutine 并发结构、事务批写、prune 安全检查、summary）。
6. **query / list / stats 命令**。
7. **集成测试** + 三平台交叉编译验证。
8. **README 补充**：安装、用法示例、平台说明。

---

## 12. 未列入首版（可后续增强）

- `--json` 结构化输出
- 已删除文件的软删除/历史版本追踪
- 多哈希算法可选（blake3 等）
- 并发写冲突下的多进程安全（当前 WAL + busy_timeout 已够单进程场景）
- 跟随 symlink 选项（`--follow-symlinks`）
- 进度条 / TUI
- stat/hash TOCTOU 防护（re-stat + 有限重试；v1 接受偶发不一致，下次扫描自愈）
- `query --path` 接受 OS 原生路径分隔符（自动转 slash）
