---
name: versioning-policy
description: 版本号 bump 规则 — PATCH=小修 / MINOR=大修 / MAJOR=仅用户明确要求；每次改动后自动 bump 并更新 CHANGELOG.md
metadata:
  type: feedback
---

版本号存储在 `main.go` 的 `var version` 常量（当前 `1.0.0`）。`CHANGELOG.md` 是完整历史记录。三段式语义化版本 `MAJOR.MINOR.PATCH`，**每次有效改动后都要主动 bump `main.go` 的 version 并在 `CHANGELOG.md` 追加条目**，无需用户提醒。

- **PATCH** (`0.1.0 → 0.1.1`) — 小修：bug fix、文档订正、默认排除规则微调、依赖 patch 升级等**不改变外部行为契约**的改动。这是默认档。
- **MINOR** (`0.1.0 → 0.2.0`) — 大修：功能新增或行为变更（新增子命令、新增输出字段、扫描逻辑扩展、数据库 schema 向后兼容新增），仍向后兼容。
- **MAJOR** (`0.x.x → 1.0.0`) — **只在用户明确要求时**才升，用于不兼容变更（DB schema 破坏性修改、CLI 接口重构）。绝不自作主张升 major。

**Why:** 用户希望可追溯的发布历史，参照 anthropic-to-sap-bridge 项目的约定建立。

**How to apply:**
- 判断改动类别 → 对应 bump `main.go` 中的 `var version` 字符串（无 `v` 前缀，如 `"0.2.0"`）。
- 同步在 `CHANGELOG.md` 顶部 `[Unreleased]` 下或直接开新版本号标题（带日期 `YYYY-MM-DD`），分 `Fixed`/`Changed`/`Added`/`Notes` 记录（中文）。
- `make build*` 通过 `-X main.version=$(git describe --tags)` 将 Git tag 注入二进制，tag 命名约定 `v0.1.0`（带 `v` 前缀）。
- 拿不准 minor 还是 patch 时：有无新增能力/行为变化？有 → minor，否 → patch。
