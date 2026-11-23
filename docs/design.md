# English

**Updated:** 2026-11-23 (maillens version 0.1.0). This describes the current code of `internal/source/outlook`, `internal/engine`, `internal/store/pg`, `internal/web` and `cmd/maillens`.

## 1. Goal

The mailbox is large (50 GB) and Outlook's own search is slow. maillens copies the *text* of the mail into PostgreSQL, where a full text index answers in milliseconds, and keeps the copy equal to the mailbox. The mail itself stays in Outlook: attachments are not copied (only their names), and "Open in Outlook" goes back to the original.

## 2. Components

```text
Outlook (COM, classic Outlook)             PostgreSQL
        |                                       ^
        v                                       |
internal/source/outlook  -->  internal/engine --> internal/store/pg  <--  internal/web  <--  browser
   (source.Source)            (compare, read,       (store.Writer,         (store.Reader)
                               apply, safeguards)    store.Reader)
                    cmd/maillens: check, migrate, sync, serve, run, search, status
```

- `internal/source` defines `Source` (stores, folders, bulk scan, read one item). `internal/source/outlook` implements it with COM; `internal/source/outlook/convert.go` holds what needs no COM and is unit tested.
- `internal/store` defines `Writer` (what a sync needs) and `Reader` (what the page needs). `internal/store/pg` implements both.
- `internal/engine` is the synchronisation. It only knows the two interfaces, so it is tested with a fake source and a fake database.
- `internal/web` is the HTTP server and the single page in `internal/web/static` (no build step, no external files).
- `internal/config`, `internal/logx`, `internal/model`, `internal/version` are small support packages.

## 3. Data model

One schema (default `maillens`, setting `postgres.schema`):

| Table | Purpose |
|---|---|
| `schema_version` | the applied migrations |
| `message` | one row per Outlook item: where it is (`store_key`, `entry_id`, `folder_path`), who and when, the text (`body`), the attachment names, and the generated `search` column |
| `sync_run` | one row per sync run with its counts, shown on the page and by `maillens status` |

`message` is unique on (`store_key`, `entry_id`). `store_key` is `mailbox:<mailbox name>` or `pst:<path of the .pst file>` in lower case. `folder_path` is Outlook's folder path without the leading `\\`, so it is unique across stores.

The `search` column is a stored generated `tsvector` of the `english` configuration with weights: subject A, sender B, recipients and attachment names C, body D. Only the first 400,000 characters of a body are indexed because a `tsvector` is limited to 1 MB; the whole body is stored. Angle brackets are turned into spaces before the text is indexed (and before the snippet is built): the parser of PostgreSQL reads `<ABC123>` as an HTML tag and drops it, together with the part number, lot number or error code that mail often puts between brackets. Indexes: GIN on `search`, B-tree on `received_at`, on `folder_path`, and on `internet_message_id`.

The index is created by `maillens migrate` (also run by `sync`, `serve` and `run`). Migrations are numbered and applied once, under a lock.

## 4. Reading Outlook

Outlook's object model is single threaded, so `outlook.Source` runs every COM call on one goroutine locked to one operating system thread (COM apartment); the methods hand their work to it.

- **Connection.** `GetActiveObject("Outlook.Application")` connects to the running Outlook; with `outlook.start_if_needed` it creates one when none runs. Both fail unless the program runs at the same privilege level as Outlook; the error says so.
- **Stores.** `Session.Stores`, kept when `ExchangeStoreType` is 0 (primary mailbox) or 3 with a `.pst` file path. Shared mailboxes, the online archive, public folders and IMAP caches are left out.
- **Folders.** The folder tree of each store, mail folders only (`DefaultItemType` = mail), without Deleted Items, Junk E-mail, Drafts, Outbox, the system folders listed in `convert.go` and the folders of `outlook.exclude_folders`. A folder that cannot be read makes the list *incomplete* (`source.ErrPartial`), which disables deletions.
- **Bulk scan.** `Folder.GetTable` with the columns of `tableColumns` returns subject, sender, times, size, Message-ID, flags and so on for every item without opening it. Only items of message class `IPM.Note*` are kept. Times come in UTC.
- **Body.** The table cuts long values, so the body (and the attachment names, only when the table said there are attachments) is read item by item with `GetItemFromID`. Numbered signature pictures (`image001.png`) are not listed as attachments.
- **Release.** Every COM object is released; a long run would otherwise leak.

## 5. The sync

One run takes the lock (a PostgreSQL advisory lock, so two programs cannot sync at once), connects to Outlook and, per store:

1. lists the folders and scans them in bulk (`current`, keyed by entry id);
2. loads what the database holds for the store (`known`, keyed by entry id);
3. builds a plan (`internal/engine/plan.go`):

| Situation | Action |
|---|---|
| entry id not stored | **read** it (new) |
| size changed | **read** it again |
| failed to read last time, and `-retry-failed` | **read** it again |
| only the modification time or folder changed (reading, flagging, categorising a mail do this) | **update** the cheap fields, no read |
| stored entry id missing from Outlook, and a new item has the same Message-ID and size | **move**: the stored row follows the item (a move gives a new entry id) |
| stored entry id missing from Outlook, no such item | **gone**: delete (section 6) |

4. applies moves and updates, then reads what is new **newest first** in batches (`sync.batch_size` messages, or 8 MB of text), then deletes what is gone.

Properties:

- **Resumable without a checkpoint.** Every batch is committed; after an interruption the next run compares again and carries on with what is missing.
- **An unreadable item** is stored with what the scan knew (subject, sender, dates) and an error text, and is not read again by accident. An item that vanished between scan and read is skipped.
- **Partial runs** (`-limit`, `-folder`, `-store`) only look at what they were asked to; `-folder` also restricts the stored side, so folders outside the filter are never deleted.
- Bodies longer than `sync.body_max_chars` are cut at a character boundary and flagged.
- `-dry-run` scans and compares but changes nothing and records no run.

## 6. Deletions and the safeguards

With `sync.mirror_deletions` (default on) a message that is gone from Outlook is removed from the index, so the index equals the mailbox. That is the dangerous part of a sync: if a store is missing (a detached `.pst`, Outlook still loading, a folder that failed), "gone from the scan" looks the same as "deleted". Three safeguards:

1. **Incomplete scan.** If the list of folders was incomplete or any folder scan failed, nothing is deleted from that store in this run. The run says so and records it.
2. **Mass deletion.** A run that would remove more than `sync.mass_delete_min` messages **and** more than `sync.mass_delete_percent` of the store is refused (`maillens sync -allow-mass-delete` applies it). The defaults are 1000 messages and 10 %.
3. **Order.** Deletions come last, after everything new was stored, so an interrupted run loses nothing.

With `mirror_deletions` off, nothing is ever removed and the index becomes an archive.

## 7. Search

The query is parsed by PostgreSQL's `websearch_to_tsquery('english', ...)`: words are combined with AND, `"a phrase"`, `a OR b`, `-excluded`. The `english` configuration stems words (attached, attaching, attach) and **drops stop words**: a query made only of words such as "the" or "after" finds nothing, and the page says so.

Three shapes (`internal/store/pg/search.go`):

- **Relevance (default with a query).** The newest `search.max_candidates` matches (default 5000) are ranked with `ts_rank_cd`, because ranking needs the whole `tsvector` of every candidate. The page and `maillens search` say when the set was cut.
- **Newest first.** Paged straight from the date index; not limited.
- **No query.** The newest messages that fit the filters.

Filters: sender (part of name or address, case insensitive), received from/until (the end day included), folder (the folder and everything below it), attachments only. Snippets come from `ts_headline`; the matches are wrapped in control characters that the page turns into `<mark>` after escaping everything else.

## 8. The web page

`internal/web` serves `/` and `/static/*` and a read-only JSON interface: `/api/search`, `/api/message/{id}`, `/api/status`, `/api/folders` (status and folders are cached for 30 s and 5 min). The page has no inline script or style (the Content-Security-Policy forbids them) and builds everything with `textContent`, except the snippet, which the server has escaped.

"Open in Outlook" is a link `outlook:<entry id>` that Windows hands to Outlook. It is best effort: it needs the item to be in the default store and to still have that entry id (a moved item gets a new one until the next sync).

## 9. Security and privacy

- Nothing leaves the machine: no network calls other than the database connection, none to the internet.
- `web.listen` must be a loopback address (validated at start); requests are only answered when the `Host` header is a loopback name (a web page of another site that resolves its own name to 127.0.0.1 is refused); only GET; no CORS headers; `Cache-Control: no-store`, `nosniff`, a restrictive Content-Security-Policy, `X-Frame-Options: DENY`.
- Errors shown in the browser say only that something failed; the details go to the log.
- Logs never contain subjects, bodies, addresses or attachment names; they contain counts, folder paths and error texts. `maillens check` prints counts and timings only.
- Connection errors are scrubbed of the password. The password may live in the environment variable `MAILLENS_PG_PASSWORD` instead of the file.
- The database holds the text of all mail in clear: protect the database and its backups like the mailbox itself (disk encryption, access rights). If your backup scripts cover the folder of the database, decide whether the index belongs in them.

## 10. Performance (expected, not yet measured on a real mailbox)

COM answers one call at a time, so the first full sync of a large mailbox is dominated by reading bodies one by one and will take hours; it is resumable and newest first, so recent mail is searchable early. The bulk scan is much faster than opening items. Later runs scan, compare and read only what changed. Searches use the GIN index; sorting by relevance is bounded by `search.max_candidates`. These are expectations from the design: measure on the real mailbox with `maillens sync -limit 200` (the log prints the rate) before the full run.

## 11. Limits

- Classic Outlook only ("new Outlook" has no COM). Outlook has to be running (or `start_if_needed`).
- The primary mailbox and attached `.pst` files only. A `.pst` that is not attached to the profile cannot be read; attach it in Outlook (File > Open & Export > Open Outlook Data File).
- English text search. Other languages are stored and shown but not stemmed, and CJK text is not segmented into words.
- Attachment contents are not indexed (names only). Meeting requests and other non-mail items are not indexed.
- HTML mail is indexed as the plain text Outlook produces for it.
- The first run, and a run after Outlook's cache changed a lot, can take hours.

---

# 中文

**更新日期：** 2026-11-23（maillens 版本 0.1.0）。本文描述 `internal/source/outlook`、`internal/engine`、`internal/store/pg`、`internal/web` 和 `cmd/maillens` 的当前代码。

## 1. 目标

邮箱很大（50 GB），Outlook 自带的搜索很慢。maillens 把邮件的*文本*复制到 PostgreSQL，由全文索引在毫秒级回答，并让这份副本始终与邮箱一致。邮件本身仍留在 Outlook：附件不复制（只保存文件名），"在 Outlook 中打开"会回到原邮件。

## 2. 组成

```text
Outlook（COM，经典版 Outlook）             PostgreSQL
        |                                       ^
        v                                       |
internal/source/outlook  -->  internal/engine --> internal/store/pg  <--  internal/web  <--  浏览器
   (source.Source)            (比较、读取、         (store.Writer,         (store.Reader)
                               应用、保护措施)       store.Reader)
                    cmd/maillens: check, migrate, sync, serve, run, search, status
```

- `internal/source` 定义 `Source`（存储、文件夹、批量扫描、读取单封邮件）。`internal/source/outlook` 用 COM 实现它；`internal/source/outlook/convert.go` 放不需要 COM 的部分，有单元测试。
- `internal/store` 定义 `Writer`（同步需要的）和 `Reader`（网页需要的）。`internal/store/pg` 同时实现两者。
- `internal/engine` 是同步逻辑。它只认识这两个接口，所以用假数据源和假数据库测试。
- `internal/web` 是 HTTP 服务和 `internal/web/static` 里的单页（无需构建，无外部文件）。
- `internal/config`、`internal/logx`、`internal/model`、`internal/version` 是小的辅助包。

## 3. 数据模型

一个 schema（默认 `maillens`，设置项 `postgres.schema`）：

| 表 | 用途 |
|---|---|
| `schema_version` | 已应用的迁移 |
| `message` | 每个 Outlook 项目一行：位置（`store_key`、`entry_id`、`folder_path`）、发件人和时间、文本（`body`）、附件名，以及生成列 `search` |
| `sync_run` | 每次同步一行，含各项计数，显示在网页上和 `maillens status` 中 |

`message` 在（`store_key`，`entry_id`）上唯一。`store_key` 是小写的 `mailbox:<邮箱名>` 或 `pst:<.pst 文件路径>`。`folder_path` 是去掉开头 `\\` 的 Outlook 文件夹路径，所以在各存储之间唯一。

`search` 列是 `english` 配置的存储型生成 `tsvector`，带权重：主题 A，发件人 B，收件人和附件名 C，正文 D。正文只对前 400,000 个字符建索引，因为 `tsvector` 限制为 1 MB；完整正文仍会保存。建索引之前（以及生成摘要之前）会把尖括号换成空格：PostgreSQL 的解析器会把 `<ABC123>` 当成 HTML 标签丢掉，连同邮件里常放在尖括号中的型号、批号或错误代码一起丢掉。索引：`search` 上的 GIN，`received_at`、`folder_path`、`internet_message_id` 上的 B-tree。

索引由 `maillens migrate` 创建（`sync`、`serve`、`run` 也会执行）。迁移带编号，加锁后只应用一次。

## 4. 读取 Outlook

Outlook 的对象模型是单线程的，所以 `outlook.Source` 在一个锁定到一个操作系统线程（COM 单元）的 goroutine 上执行所有 COM 调用；各方法把工作交给它。

- **连接。** `GetActiveObject("Outlook.Application")` 连接正在运行的 Outlook；设置 `outlook.start_if_needed` 后，在没有运行时会创建一个。两者都要求程序与 Outlook 运行在相同的权限级别，否则会失败；错误信息会说明这一点。
- **存储。** `Session.Stores`，只保留 `ExchangeStoreType` 为 0（主邮箱）或为 3 且有 `.pst` 文件路径的。共享邮箱、联机存档、公共文件夹和 IMAP 缓存不在其中。
- **文件夹。** 每个存储的文件夹树，只取邮件文件夹（`DefaultItemType` 为邮件），不含已删除邮件、垃圾邮件、草稿、发件箱、`convert.go` 里列出的系统文件夹，以及 `outlook.exclude_folders` 里的文件夹。有文件夹读取失败时，列表被视为*不完整*（`source.ErrPartial`），这会禁用删除。
- **批量扫描。** 使用 `tableColumns` 里的列调用 `Folder.GetTable`，无需打开项目就能得到每封邮件的主题、发件人、时间、大小、Message-ID、标志等。只保留邮件类别为 `IPM.Note*` 的项目。时间是 UTC。
- **正文。** 表会截断长值，所以正文（以及附件名，仅当表显示有附件时）用 `GetItemFromID` 逐封读取。带编号的签名图片（`image001.png`）不算附件。
- **释放。** 每个 COM 对象都会释放；否则长时间运行会泄漏。

## 5. 同步

一次运行先取得锁（PostgreSQL 的 advisory lock，所以两个程序不能同时同步），连接 Outlook，然后对每个存储：

1. 列出文件夹并批量扫描（`current`，以 entry id 为键）；
2. 读取数据库中该存储已有的内容（`known`，以 entry id 为键）；
3. 生成计划（`internal/engine/plan.go`）：

| 情形 | 动作 |
|---|---|
| entry id 未存储 | **读取**（新邮件） |
| 大小变了 | 再次**读取** |
| 上次读取失败，并且使用了 `-retry-failed` | 再次**读取** |
| 只有修改时间或文件夹变了（阅读、标记、分类邮件都会这样） | **更新**廉价字段，不读取 |
| 已存储的 entry id 在 Outlook 中不存在，并且有一封新邮件的 Message-ID 和大小相同 | **移动**：已存储的行跟随该邮件（移动会产生新的 entry id） |
| 已存储的 entry id 在 Outlook 中不存在，也没有这样的邮件 | **已消失**：删除（第 6 节） |

4. 先应用移动和更新，再按**最新的在前**分批读取新邮件（每批 `sync.batch_size` 封，或 8 MB 文本），最后删除已消失的。

特性：

- **无需检查点即可续传。** 每一批都已提交；中断之后，下一次运行重新比较，继续处理缺少的部分。
- **读不了的邮件**会连同扫描得到的信息（主题、发件人、日期）和一段错误文字一起保存，不会被意外地再读一遍。在扫描和读取之间消失的邮件会被跳过。
- **局部运行**（`-limit`、`-folder`、`-store`）只看被要求的范围；`-folder` 同时限制已存储的一侧，所以过滤范围之外的文件夹绝不会被删除。
- 超过 `sync.body_max_chars` 的正文会在字符边界处截断并做标记。
- `-dry-run` 只扫描和比较，不改动任何东西，也不记录运行。

## 6. 删除与保护措施

启用 `sync.mirror_deletions`（默认开启）后，在 Outlook 中消失的邮件会从索引中删除，使索引与邮箱一致。这是同步里最危险的部分：如果某个存储不在了（被卸载的 `.pst`、Outlook 还在加载、某个文件夹读取失败），"扫描里没有"看起来与"已删除"一模一样。三项保护：

1. **扫描不完整。** 如果文件夹列表不完整，或任何文件夹扫描失败，本次运行不会从该存储删除任何东西。运行结果会说明并记录这一点。
2. **大批量删除。** 一次运行如果要删除超过 `sync.mass_delete_min` 封**并且**超过该存储 `sync.mass_delete_percent` 的邮件，会被拒绝（`maillens sync -allow-mass-delete` 可强制执行）。默认值是 1000 封和 10 %。
3. **顺序。** 删除放在最后，在所有新邮件都已存储之后，所以被中断的运行不会丢失任何东西。

关闭 `mirror_deletions` 后，永远不会删除任何东西，索引就成了存档。

## 7. 搜索

查询由 PostgreSQL 的 `websearch_to_tsquery('english', ...)` 解析：多个词按 AND 组合，支持 `"短语"`、`a OR b`、`-排除词`。`english` 配置会做词干化（attached、attaching、attach），并且**丢弃停用词**：只由 "the"、"after" 这类词组成的查询什么也找不到，网页会提示。

三种形态（`internal/store/pg/search.go`）：

- **按相关度（有查询时的默认）。** 对最新的 `search.max_candidates` 个匹配（默认 5000）用 `ts_rank_cd` 排序，因为排序需要每个候选项完整的 `tsvector`。结果被截断时，网页和 `maillens search` 会说明。
- **最新的在前。** 直接用日期索引分页，不受限制。
- **无查询。** 符合过滤条件的最新邮件。

过滤：发件人（姓名或地址的一部分，不区分大小写）、收到日期的起止（包含结束那天）、文件夹（该文件夹及其下的所有文件夹）、仅含附件。摘要来自 `ts_headline`；匹配处被包在控制字符里，网页在转义其余所有内容之后把它们换成 `<mark>`。

## 8. 网页

`internal/web` 提供 `/`、`/static/*` 和只读的 JSON 接口：`/api/search`、`/api/message/{id}`、`/api/status`、`/api/folders`（状态和文件夹分别缓存 30 秒和 5 分钟）。页面没有内联脚本和样式（Content-Security-Policy 禁止），除服务器已转义的摘要之外，所有内容都用 `textContent` 构建。

"在 Outlook 中打开"是一个 `outlook:<entry id>` 链接，由 Windows 交给 Outlook。这是尽力而为：邮件需要在默认存储中，并且还要有这个 entry id（被移动过的邮件在下次同步之前有的是新 id）。

## 9. 安全与隐私

- 任何内容都不离开本机：除数据库连接外没有其他网络调用，更没有访问互联网。
- `web.listen` 必须是回环地址（启动时校验）；只有 `Host` 头是回环名称的请求才会被应答（别的网站把自己的域名解析到 127.0.0.1 的网页会被拒绝）；只接受 GET；没有 CORS 头；`Cache-Control: no-store`、`nosniff`、严格的 Content-Security-Policy、`X-Frame-Options: DENY`。
- 浏览器里显示的错误只说明出了问题；细节写进日志。
- 日志里绝不包含主题、正文、地址或附件名；只有数量、文件夹路径和错误文字。`maillens check` 只输出数量和耗时。
- 连接错误会去掉密码。密码可以放在环境变量 `MAILLENS_PG_PASSWORD` 里，而不是文件里。
- 数据库里是全部邮件的明文：要像保护邮箱一样保护数据库及其备份（磁盘加密、访问权限）。如果你的备份脚本覆盖了数据库所在的文件夹，请决定索引是否应该被备份。

## 10. 性能（预期，尚未在真实邮箱上测量）

COM 一次只回答一个调用，所以大邮箱的第一次完整同步主要耗在逐封读取正文上，会花几个小时；它可以续传，并且最新的在前，所以最近的邮件很早就能搜到。批量扫描比逐个打开项目快得多。之后的运行只扫描、比较并读取有变化的部分。搜索使用 GIN 索引；按相关度排序受 `search.max_candidates` 限制。这些是设计上的预期：完整运行之前，先用 `maillens sync -limit 200` 在真实邮箱上测量（日志会打印速率）。

## 11. 限制

- 只支持经典版 Outlook（"新版 Outlook"没有 COM）。Outlook 必须在运行（或设置 `start_if_needed`）。
- 只支持主邮箱和已挂载的 `.pst` 文件。没有挂载到配置文件的 `.pst` 读不了；请在 Outlook 里挂载（文件 > 打开和导出 > 打开 Outlook 数据文件）。
- 英文全文检索。其他语言会保存和显示，但不做词干化，中日文也不会切分成词。
- 不索引附件内容（只有文件名）。会议请求和其他非邮件项目不索引。
- HTML 邮件按 Outlook 为它生成的纯文本建索引。
- 第一次运行，以及 Outlook 缓存大量变化之后的运行，可能要花几个小时。
