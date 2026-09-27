# Changelog

本项目版本号遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/) 的三段式 `MAJOR.MINOR.PATCH`。

**升级规则（本项目约定）**：

- **PATCH**（`0.1.0 → 0.1.1`）— 小 bug 修复、文档订正、默认排除规则微调、依赖 patch 升级等不影响外部行为契约的改动。
- **MINOR**（`0.1.0 → 0.2.0`）— 较大功能新增或行为变更（新增子命令、新增输出字段、扫描逻辑扩展等）。仍需保持向后兼容。
- **MAJOR**（`0.x.x → 1.0.0`）— **仅在用户明确要求时**才升级；用于不兼容变更（数据库 schema 破坏性修改、命令行接口重构等）。

发布时把 `[Unreleased]` 下的条目移到新版本号标题下并写上日期。

版本号同步位置：`main.go` 的 `var version` 常量；构建时 `make build` 优先用 `git describe --tags` 覆盖（已在 Makefile 配置好）。

---

## [Unreleased]

（暂无未发布变更）

---

## [1.1.0] — 2026-09-27

### Added

- **自动升级检查** — 每次启动时向 GitHub Releases API 查询最新版本（5 s 超时），若有新版本则提示用户是否升级。确认后：下载当前 OS/arch 对应的二进制 → 备份现有文件（`.bak`）→ 以 `fshash` 命名安装到同目录 → 运行 `fshash --version` 验证 → 删除备份。验证失败时自动从备份还原。
- **`--skip-upgrade-check` 全局 flag** — 跳过启动时的版本检查，适用于脚本/CI 场景。
- **GitHub Actions release workflow** — 手动触发（`workflow_dispatch`），从 `main.go` 读取版本号，构建三平台静态二进制，生成 `checksums.txt`，创建 GitHub Release。

---

## [1.0.0] — 2026-09-27

### Added

- **文件 SHA-256 扫描** — `fshash scan <dir>` 递归扫描目录，计算每个文件的 SHA-256 哈希并存入 SQLite 数据库。支持 `--concurrency`（默认 8 个并发 worker）、`--exclude`（追加正则排除规则）、`--no-default-excludes`、`--prune`（删除已消失文件的记录）。
- **增量扫描** — 对比文件的 mtime（纳秒精度）与 size；未变化的文件直接更新 `scan_id`，跳过重复哈希，大幅提升重复扫描速度。
- **文件夹哈希** — 扫描完成后自底向上为每个含文件的目录计算内容哈希（SHA-256 of sorted direct-children hashes）。文件名、属性、时间戳均**不**纳入计算，仅反映内容变化；子文件/子目录增删改均会触发祖先目录哈希更新。`--prune` 时同步清理过期文件夹记录。
- **`fshash list`** — 列出数据库中的文件记录，支持 `--root`（按扫描根过滤）、`--limit`、`--offset` 分页。
- **`fshash query`** — 按 `--hash`（精确哈希）、`--name`（文件名/路径 LIKE 模式）、`--path`（精确相对路径）查询记录。
- **`fshash stats`** — 显示总文件数、总大小、重复哈希组数等聚合统计。
- **默认排除规则** — 内置排除 `node_modules/`、`.git/`、`__pycache__/`、`.DS_Store`、`*.pyc` 等常见噪音目录/文件；数据库自身文件（`fshash.db` 及 WAL/SHM）自动跳过。
- **跨平台构建** — `make build-all` 生成 `darwin-arm64`、`linux-amd64`、`linux-arm64` 三个静态二进制（`CGO_ENABLED=0`）。
- **`--version` 标志** — 显示当前版本号；`make build*` 通过 `-X main.version=$(git describe --tags)` 自动将 Git tag 注入二进制。
