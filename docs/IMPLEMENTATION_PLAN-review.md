# IMPLEMENTATION_PLAN Review

审查范围：根据 `README.md` 检查 `docs/IMPLEMENTATION_PLAN.md` 的需求一致性、数据正确性、实现风险和测试完整性。

## Review 发现

### 1. 高：默认数据库可能被扫描并参与哈希，造成自引用和结果不稳定

`docs/IMPLEMENTATION_PLAN.md:160` 将数据库默认设为 `./fshash.db`，而 `docs/IMPLEMENTATION_PLAN.md:177-204` 会扫描目标目录中的所有文件。如果执行 `fshash scan .`，数据库本身以及 SQLite 的 `-wal`、`-shm` 文件可能被扫描；写数据库又会改变它们，导致保存的哈希立即失效，甚至出现持续变化或文件访问冲突。

建议：

- 始终排除数据库主文件及其 `-wal`、`-shm` 文件。
- 比较 canonical absolute path，而不是只依赖 regex。
- 最好将默认数据库放到用户数据目录，而不是当前目录。

### 2. 高：Unix 秒级 `modified_ts` 不足以可靠判断文件未变化

`docs/IMPLEMENTATION_PLAN.md:78` 和 `docs/IMPLEMENTATION_PLAN.md:111-113` 使用 Unix 秒，并以 `size + modified_ts` 判断是否跳过哈希。同尺寸文件如果在一秒内被修改，重扫时可能错误跳过。

建议将时间保存为 Unix nanoseconds，例如 `fi.ModTime().UnixNano()`；字段名也可改为 `modified_ns`，避免单位不明确。

### 3. 高：`--prune` 在扫描不完整时可能误删有效记录

`docs/IMPLEMENTATION_PLAN.md:155`、`docs/IMPLEMENTATION_PLAN.md:200-201` 没有定义扫描出错时的 prune 行为。如果目录无权限、文件遍历失败、context 被取消，未访问到的记录会被当成“已删除”。测试计划虽然在 `docs/IMPLEMENTATION_PLAN.md:251` 提到无权限文件，却未定义安全语义。

建议只有在整次遍历成功完成后才能 prune；任何 traversal error、取消或 worker error 都应禁止 prune，并返回非零退出码。

### 4. 高：`last_seen_ts` 使用时间戳作为扫描代次不够安全

`docs/IMPLEMENTATION_PLAN.md:82`、`docs/IMPLEMENTATION_PLAN.md:180`、`docs/IMPLEMENTATION_PLAN.md:193` 使用 `scanStart := now()` 标识当前扫描。如果采用秒级时间，两次扫描在同一秒内启动，旧记录可能与本轮记录拥有相同的 `last_seen_ts`，从而逃过 prune。系统时间回拨也会破坏该逻辑。

建议使用唯一 `scan_id`，例如在 `scans` 表中创建自增 ID；不要用 wall-clock timestamp 充当扫描代次。

### 5. 中高：文件可能在 `stat` 与哈希读取之间变化

`docs/IMPLEMENTATION_PLAN.md:190-198` 先读取 metadata，再异步计算哈希。文件在两步之间被修改时，数据库可能保存“旧 metadata + 新内容或部分内容 hash”的不一致组合。

建议哈希完成后再次 `stat`，比较 size 和 nanosecond mtime；变化则重试有限次数，持续变化则记录错误并跳过写入。

### 6. 中高：伪代码中的 worker/result 消费顺序存在死锁风险

`docs/IMPLEMENTATION_PLAN.md:182-198` 在 `WalkDir` 中向 worker pool 提交任务，但直到遍历结束后才消费 `results`。如果 jobs 和 results 都是有界 channel，workers 可能阻塞于写满的 results channel，遍历器又阻塞于写 jobs channel。

建议文档明确：

- collector 与 `WalkDir`、workers 同时运行；
- 谁关闭 jobs/results channel；
- error propagation 和 context cancellation；
- DB 写入由单一 goroutine 负责。

### 7. 中：Linux `ctime` 不能满足 README 的“创建时间”要求

README 要求 create timestamp，见 `README.md:17`。计划在 `docs/IMPLEMENTATION_PLAN.md:210-213` 将 Linux `Ctim` 当作替代值，但 `ctime` 是 inode metadata change time，不是创建时间；回退到 mtime 同样不是创建时间。数据库字段仍叫 `created_ts`，容易让查询方误解。

建议：

- 支持时读取 birth time；
- 不支持时保存 `NULL`；
- 或增加 `created_time_source`；
- 不要把 ctime/mtime 无标记地存成 creation time。

### 8. 中：SQLite PRAGMA 与连接池行为没有定义清楚

`docs/IMPLEMENTATION_PLAN.md:115-120` 表示“打开后设置”PRAGMA，但 `database/sql` 可能建立多个连接，其中部分 PRAGMA 是 per-connection。并发查询与批量写入时，行为未必符合预期。

建议明确使用 DSN 参数、driver connection hook，或设置 `SetMaxOpenConns(1)` 并采用单 writer 架构。还应定义事务失败、busy timeout 和 rollback 行为。

### 9. 中：扫描对象类型和 symlink 语义不够明确

`docs/IMPLEMENTATION_PLAN.md:216` 说“不跟随 symlink”，但 `WalkDir` 只是不自动递归进入 symlink directory；后续如果对 symlink 调用 `os.Stat`，仍可能跟随目标。socket、FIFO、device 等特殊文件也未定义。

建议首版只处理 `DirEntry.Type().IsRegular()`；明确 symlink 是跳过、记录链接本身，还是跟随目标。

### 10. 中：扫描根路径的身份规范化不足

`docs/IMPLEMENTATION_PLAN.md:177` 只使用 `abspath(dir)`。同一个目录可能通过 symlink、`..`、不同大小写路径或挂载别名访问，最终在同一 DB 中形成多个 `scan_root`。

建议至少使用 `filepath.Abs` + `filepath.Clean`，并明确是否使用 `filepath.EvalSymlinks`。macOS 大小写语义也需要说明。

### 11. 中：“已确认”的技术决策实际上无法从 README 推导出来

`docs/IMPLEMENTATION_PLAN.md:21-29` 将 SQLite、Cobra、默认排除规则、worker pool 标为“已确认”，但 README 只确认了 Go、file-based DB、SHA-256、regex 排除和增量条件。`query/list/stats`、`--prune` 等也是新增设计。

建议改为“建议技术决策”或增加 decision status，例如 `proposed / accepted`，避免把生成文档中的推断误认为用户需求。

### 12. 低：默认排除规则没有完整、可验证的定义

`docs/IMPLEMENTATION_PLAN.md:137` 只展示 `node_modules`、`.git` 和省略号。省略号无法转成验收测试，也不清楚是否会默认排除 `.fshash.db`、构建产物、隐藏目录等。

建议列出完整默认规则，并说明：

- regex 匹配的是 slash-normalized relative path；
- 是否区分大小写；
- 无效 regex 的退出行为；
- 用户规则是追加还是覆盖默认规则。

### 13. 低：文档缺少 CLI 契约和验收标准

`docs/IMPLEMENTATION_PLAN.md:158-169` 有命令概览，但没有明确 exit code、错误输出到 stdout/stderr、排序稳定性、路径查询是精确还是模糊匹配、重复 hash 的定义，以及扫描部分失败时是否仍提交成功结果。

建议为每个命令补充最小 CLI contract 和可验证 acceptance criteria。

## 总体评价

文档结构、模块拆分、schema、平台构建目标和测试层次都比较清楚，作为初始实现计划是可用的。但在开始编码前，建议优先解决前五项，否则可能出现错误跳过、错误 prune、自扫描数据库及 metadata/hash 不一致等数据正确性问题。

目前 `docs/` 只有 `IMPLEMENTATION_PLAN.md`，仓库中没有文档生成配置，因此它更像一次性生成的设计稿，而不是可重复生成的 documentation。建议明确它是 source-of-truth 设计文档还是生成产物，并在确认后纳入版本控制。
