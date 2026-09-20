# fs-hash-util — 详细实现计划

> 本文档为实现计划（design & plan），**不含实现代码**。据 `README.md` 需求编写。
> 生成日期：2026-09-20

---

## 1. 项目目标

用 Go 实现一个**本地文件哈希工具**：扫描指定目录（含子目录）下所有文件，计算 SHA-256 哈希，把结果存入**文件型数据库**，供后续查询与维护。重扫描时对未变更文件跳过重算。

### 交付物

- 单一可执行二进制 `fshash`，交叉编译支持：
  - `darwin/arm64`（macOS Apple Silicon）
  - `linux/amd64`（ubuntu x64）
  - `linux/arm64`（ubuntu arm64）

---

## 2. 关键技术决策（已确认）

| 决策项 | 选定方案 | 理由 |
|--------|----------|------|
| DB 引擎 | **SQLite**（`modernc.org/sqlite`，纯 Go / CGO-free） | 单文件、支持 SQL 查询与索引、事务、增量 upsert；纯 Go 驱动可无痛交叉编译到三平台 |
| CLI 框架 | **`spf13/cobra`** 子命令式 | `fshash scan / query / list / stats`，扩展性好 |
| 排除规则 | **内置默认 + `--exclude` regex flag** | 开箱即用（默认排 `node_modules`、`.git` 等）又可自定义 |
| 哈希算法 | **SHA-256**（`crypto/sha256`，流式读取） | README 指定；空文件用 dummy 值 |
| 并发 | worker pool（`errgroup` + 有界并发） | 大目录下加速哈希计算 |

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
    │   ├── model.go            # FileRecord 结构体
    │   └── repository.go       # CRUD：Upsert / Get / List / Query / Exists
    ├── scanner/
    │   ├── scanner.go          # 目录遍历 + 排除规则 + 增量判断
    │   └── exclude.go          # 默认排除模式 + regex 匹配
    └── hasher/
        └── hasher.go           # SHA-256 流式哈希，空文件 dummy 值
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
| `CreatedTS` | `int64` | 文件创建时间戳（Unix 秒；见 §7 平台差异说明） |
| `ModifiedTS` | `int64` | 文件最后修改时间戳（Unix 秒） |
| `Size` | `int64` | 文件字节数 |
| `Hash` | `string` | SHA-256 十六进制；`Size==0` 时用固定 dummy 值 |
| `ScanRoot` | `string` | 本条记录所属的扫描根（绝对路径），支持一个 DB 存多个 root |
| `LastSeenTS` | `int64` | 本条记录最近一次被扫描确认存在的时间（用于检测已删除文件） |

### 4.2 SQL Schema（`scan` 首次运行时建表）

```sql
CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_root    TEXT    NOT NULL,
    file_name    TEXT    NOT NULL,
    rel_path     TEXT    NOT NULL,
    created_ts   INTEGER NOT NULL,
    modified_ts  INTEGER NOT NULL,
    size         INTEGER NOT NULL,
    hash         TEXT    NOT NULL,
    last_seen_ts INTEGER NOT NULL,
    UNIQUE(scan_root, rel_path)
);

CREATE INDEX IF NOT EXISTS idx_files_hash     ON files(hash);
CREATE INDEX IF NOT EXISTS idx_files_relpath  ON files(rel_path);

-- schema 版本管理
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- 初始化写入 schema_version=1
```

**增量跳过键**：README 要求"same relative-path + file-name + file-size + last-update-time 视为未变更"。
- `rel_path` 已含 file-name，故唯一约束用 `(scan_root, rel_path)`。
- 跳过判定：查现有记录，若 `size` 与 `modified_ts` 均相同 → 跳过哈希计算，仅更新 `last_seen_ts`。

**DB 连接 PRAGMA**（打开后设置）：
```sql
PRAGMA journal_mode=WAL;     -- 并发读写更稳
PRAGMA synchronous=NORMAL;   -- 性能与安全折中
PRAGMA busy_timeout=5000;
```

---

## 5. 模块设计

### 5.1 `internal/hasher`

- `HashFile(path string) (string, error)`：`os.Open` → `io.Copy` 到 `sha256.New()` → 返回 hex。
- **空文件**（`size == 0`）：直接返回固定 dummy 值常量，例如
  `const EmptyFileHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"`
  （实为空串 SHA-256）或自定义 `"EMPTY"` 哨兵——**方案选定：用真实空串 SHA-256**，语义正确且可与非空文件统一比较。
- 流式读取，避免大文件一次性载入内存。

### 5.2 `internal/scanner`

- `exclude.go`：
  - `DefaultExcludes = []string{"(^|/)node_modules(/|$)", "(^|/)\\.git(/|$)", ...}`
  - `Matcher`：编译用户 `--exclude` regex + 默认（除非 `--no-default-excludes`）→ `[]*regexp.Regexp`。
  - `ShouldSkip(relPath string, isDir bool) bool`：任一 regex 命中即跳过；命中目录时整个子树剪枝。
- `scanner.go`：
  - `Scan(ctx, root string, opts) (<-chan FileRecord, <-chan error)` 或收集式 API。
  - 用 `filepath.WalkDir`（比 `Walk` 快，少 `Lstat`）。
  - 目录被排除 → 返回 `fs.SkipDir` 剪枝。
  - 对每个文件取 `os.Stat`（size、mtime、ctime）→ 交给增量判断 → 需要则 `hasher.HashFile`。
  - **相对路径规范化**：`filepath.Rel(root, path)` 后 `filepath.ToSlash()` 统一为 `/`，保证跨平台一致。

### 5.3 `internal/db`

- `Open(path string) (*DB, error)`：`sql.Open("sqlite", path)` + PRAGMA + `migrate()`。
- `repository.go`：
  - `GetByRelPath(scanRoot, relPath) (*FileRecord, bool, error)` — 增量判断用。
  - `Upsert(rec FileRecord) error` — `INSERT ... ON CONFLICT(scan_root, rel_path) DO UPDATE`。
  - `List(scanRoot string, limit, offset int) ([]FileRecord, error)`。
  - `QueryByHash(hash string) ([]FileRecord, error)`。
  - `MarkStale/DeleteNotSeen(scanRoot, scanStartTS)` — 可选：清理本轮未见（已删除）文件。
  - 批量写用**事务**（每 N 条 commit 一次）提升吞吐。

### 5.4 `cmd`（cobra）

**全局 flag（root.go）**：`--db <path>`（默认 `./fshash.db`）、`--verbose`。

| 子命令 | 用法 | 行为 |
|--------|------|------|
| `scan` | `fshash scan <dir> [--db f] [--exclude re]... [--no-default-excludes] [--concurrency N] [--prune]` | 扫描目录写入/更新 DB；`--prune` 删除本轮未见记录 |
| `query` | `fshash query --hash <sha256>` 或 `--path <relpath>` | 按哈希或路径查询并打印 |
| `list` | `fshash list [--root <dir>] [--limit N] [--offset N]` | 列出记录（表格输出） |
| `stats` | `fshash stats` | 打印文件总数、总大小、重复哈希数等汇总 |

输出格式：默认人类可读表格；可加 `--json` flag 输出 JSON（后续增强，非首版必需）。

---

## 6. scan 主流程（伪逻辑）

```
scan(dir):
  root := abspath(dir)
  db := open(dbPath); db.migrate()
  matcher := buildExcludeMatcher(defaults, userExcludes)
  scanStart := now()

  results := worker pool (concurrency=N)
  WalkDir(root):
    for each entry:
      rel := toSlash(rel(root, path))
      if matcher.ShouldSkip(rel, isDir):
          if isDir: return SkipDir   # 剪枝
          else: continue
      if isDir: continue
      stat := stat(path)
      existing, found := db.GetByRelPath(root, rel)
      if found && existing.size==stat.size && existing.modified==stat.mtime:
          db.touchLastSeen(existing.id, scanStart)   # 跳过哈希
          continue
      submit path to worker pool → hash

  for rec in results (batched in a transaction):
      db.Upsert(rec)

  if --prune:
      db.DeleteNotSeen(root, scanStart)

  print summary (scanned / hashed / skipped / errors)
```

---

## 7. 平台差异与注意点

- **创建时间（CreatedTS）**：Go 标准库无跨平台创建时间 API。
  - 方案：通过 `os.Stat` 的 `Sys()` 断言到 `*syscall.Stat_t`，Linux 用 `Ctim`（实为 change time，无真正 birth time），macOS(darwin) 用 `Birthtimespec`。
  - 用**构建标签分文件**：`stat_darwin.go` / `stat_linux.go` 各实现 `birthTime(fi os.FileInfo) int64`。
  - 若平台取不到 birth time，则回退用 `ModTime`，并在文档注明。
- **修改时间**：`fi.ModTime()` 跨平台可靠，直接用。
- **路径分隔符**：DB 内统一存 `/`（`filepath.ToSlash`），保证不同 OS 生成的 DB 可互查。
- **符号链接**：`WalkDir` 默认不跟随 symlink（安全）；首版按此，文档注明。
- **CGO_ENABLED=0**：必须，否则 modernc 优势失效且交叉编译失败。

---

## 8. 交叉编译（Makefile 目标）

```
build-darwin-arm64:  GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o dist/fshash-darwin-arm64
build-linux-amd64:   GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -o dist/fshash-linux-amd64
build-linux-arm64:   GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -o dist/fshash-linux-arm64
build-all: build-darwin-arm64 build-linux-amd64 build-linux-arm64
```

加 `-ldflags "-s -w"` 减小体积；可注入版本号 `-X main.version=...`。

---

## 9. 依赖清单（go.mod）

- `github.com/spf13/cobra` — CLI
- `modernc.org/sqlite` — 纯 Go SQLite 驱动
- `golang.org/x/sync/errgroup` — 并发 worker pool
- （标准库：`crypto/sha256`、`io/fs`、`path/filepath`、`database/sql`、`regexp`）

---

## 10. 测试计划

| 层级 | 内容 |
|------|------|
| 单元 | `hasher`：普通文件 / 空文件 dummy 值；`scanner/exclude`：默认与自定义 regex 命中；路径规范化 |
| 单元 | `db`：Upsert 幂等、唯一约束冲突走 update、QueryByHash |
| 集成 | 临时目录（`t.TempDir()`）造文件树 → scan → 断言 DB 记录；重扫断言跳过计数；改文件后重扫断言重算 |
| 集成 | 排除 `node_modules` 生效（含子树剪枝）；空文件处理 |
| 边界 | 无权限文件、扫描中文件被删、超大文件流式（可选） |

CI（可选）：GitHub Actions matrix 跑三平台 `go build` + `go test`。

---

## 11. 实现阶段（建议顺序）

1. **脚手架**：`go mod init`、`main.go`、cobra root + 空子命令、Makefile。
2. **hasher**：SHA-256 + 空文件处理 + 单测。
3. **db**：Open/migrate/model/repository + 单测。
4. **scanner**：WalkDir + exclude + 增量判断 + 平台 birth time 文件。
5. **scan 命令**：串起 scanner→hasher→db（含 worker pool、事务批写、summary）。
6. **query / list / stats 命令**。
7. **集成测试** + 三平台交叉编译验证。
8. **README 补充**：安装、用法示例、平台说明。

---

## 12. 未列入首版（可后续增强）

- `--json` 结构化输出
- 已删除文件的软删除/历史版本追踪
- 多哈希算法可选（blake3 等）
- 并发写冲突下的多进程安全（当前 WAL + busy_timeout 已够单进程场景）
- 跟随 symlink 选项
- 进度条 / TUI
