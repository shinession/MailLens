# English

# MailLens

Search a large Outlook mailbox in milliseconds. MailLens copies the text of your mail from the local Outlook into PostgreSQL, keeps the copy equal to the mailbox, and gives you a search page and a command line on top of PostgreSQL full text search. The mail itself stays in Outlook; everything stays on your computer.

Current version: **0.1.0** (see `VERSION`).

## Status: what has and has not been verified

- **Verified (2026-11-23):**
  - unit tests of the configuration, of the conversion of Outlook rows, of the sync logic (with a fake Outlook and a fake database: new, changed, moved, deleted and unreadable mail, interrupted runs, incomplete scans, the deletion safeguards) and of the web interface;
  - 10 integration tests against a real PostgreSQL 16.4 (a throw-away server): schema, ranking, filters, snippets, paging, moves, deletions, locks, odd text;
  - an end-to-end run of `bin\maillens.exe` against that server (40 checks): commands, error messages, the search page and its interface, and `maillens run` staying up while Outlook cannot be reached.
- **Not verified: the COM adapter against a live Outlook** (`internal/source/outlook`, the part that reads the mail). It was written from the documentation of the Outlook object model and the sources of `go-ole`, and the session that built it ran as administrator, which Outlook's COM interface refuses. So run `maillens check` first, then `maillens sync -limit 200`, in a normal terminal, and read the log: expect to correct details there. The speed on a 50 GB mailbox has not been measured.

## Requirements

- Windows with **classic** Outlook (Microsoft 365 Apps, or Outlook 2016 and newer), running. The "new Outlook" has no COM interface and cannot be read.
- maillens must be started at the **same privilege level as Outlook**: from a normal terminal, not "Run as administrator".
- PostgreSQL 12 or newer (tested with 16.4), and a database with a user that may create a schema in it.
- To build: Go 1.27.

## Setup

1. Create the database, once (pgAdmin, or `psql` from the `bin` folder of PostgreSQL):

   ```sql
   CREATE ROLE maillens LOGIN PASSWORD '<choose a password>';
   CREATE DATABASE maillens OWNER maillens;
   ```

2. Configuration: copy `config\maillens.example.json` to `config\maillens.json` and edit it. Give the password in `postgres.password`, or in the environment variable `MAILLENS_PG_PASSWORD`. The file is ignored by git.
3. Build: `.\scripts\build.ps1` (runs `go vet` and `go test`, then builds `bin\maillens.exe`).
4. In a **normal** terminal, with Outlook running: `bin\maillens.exe check`. With `-v` it lists the stores and folders that would be indexed. It prints counts and timings, never mail text.
5. First test: `bin\maillens.exe sync -limit 200`, then `bin\maillens.exe search <word>`.
6. Full sync: `bin\maillens.exe sync`. For a large mailbox this takes hours. It can be stopped with Ctrl+C and continued later (it carries on with what is missing), the newest mail comes first, and `sync.pause_ms` slows it down to keep Outlook responsive.
7. Daily use: `bin\maillens.exe run` serves the search page and syncs every `sync.interval_minutes`. To start it at log on, create a task in the Task Scheduler with the trigger "At log on", "Run only when user is logged on", and **without** "Run with highest privileges".

Open `http://127.0.0.1:8765/` (or the address in `web.listen`).

## Commands

| Command | What it does |
|---|---|
| `check [-v]` | tests the configuration, PostgreSQL and the connection to Outlook (reads a few rows and one message to time it) |
| `migrate` | creates or updates the tables (`sync`, `serve` and `run` do it too) |
| `sync` | copies mail from Outlook into PostgreSQL; flags: `-limit N`, `-store text`, `-folder text`, `-retry-failed`, `-dry-run`, `-allow-mass-delete` |
| `serve` | serves the search page (needs no Outlook) |
| `run` | `serve` plus a sync every `sync.interval_minutes`, until Ctrl+C; when Outlook cannot be reached the page still works and the sync is tried again |
| `search <query>` | searches from the command line; flags: `-from`, `-since`, `-until`, `-folder`, `-attach`, `-sort relevance\|date`, `-n`, `-page` |
| `status` | what the index holds and how the last sync went |
| `version` | prints the version |

Every command takes `-config <file>` (default: `config\maillens.json` next to the program, or one folder above it).

## Searching

Words are combined with AND; `"an exact phrase"`, `a OR b` and `-excluded` work. The search is English: words are reduced to their stem (attached, attaching, attach), and very common words such as "the" or "after" are ignored, so a query made only of them finds nothing. Part numbers, lot numbers and error codes are ordinary words; text between angle brackets (`<ABC123>`) is searchable too. Sender, subject, recipients, attachment names and the body are searched; the subject weighs most. Filters: sender, received since/until, folder (with everything below it), only mail with attachments. Sort by relevance (ranks the newest `search.max_candidates` matches) or newest first.

## What is indexed

- The primary Exchange mailbox and the `.pst` files attached to your Outlook. Shared mailboxes, the online archive, public folders and IMAP caches are not indexed. A `.pst` that is not attached to Outlook cannot be read: attach it in Outlook (File > Open & Export > Open Outlook Data File).
- All mail folders except Deleted Items, Junk E-mail, Drafts, Outbox, system folders (Sync Issues, RSS Feeds, Conversation History and the like) and the folders in `outlook.exclude_folders`. Calendar, contacts and tasks are not mail and are skipped.
- Per message: folder, sender, recipients (as shown by Outlook), dates, size, importance, attachment **names**, and the body as plain text (cut at `sync.body_max_chars`). Attachment contents are not copied or indexed.

## Keeping the index equal to the mailbox

A message that is gone from Outlook is removed from the index (`sync.mirror_deletions`, on by default). Because a missing store looks the same as deleted mail, there are safeguards: nothing is deleted from a store whose scan was incomplete, and a run that would remove more than `sync.mass_delete_min` messages and more than `sync.mass_delete_percent` of the store is refused (`maillens sync -allow-mass-delete` applies it anyway). Moving a mail to another folder is recognised and does not read it again; reading or flagging a mail does not either. With `mirror_deletions` off nothing is ever removed and the index becomes an archive. Details: `docs/design.md`.

## Privacy and security

- Nothing leaves your computer. maillens makes no network connection except to your PostgreSQL.
- The search page listens on a loopback address only (other values of `web.listen` are refused), answers only requests addressed to a loopback name, only reads, and sets no CORS headers.
- Logs and error messages contain counts, folder paths and error codes, never subjects, bodies or addresses.
- **The database holds the text of all your mail in clear.** Protect the database and its backups like the mailbox (disk encryption, access rights). If your backup scripts cover the folder of the database, decide whether the index belongs in them.

## Configuration (`config\maillens.json`)

A key that is left out keeps its default; an unknown key is an error.

| Key | Default | Meaning |
|---|---|---|
| `postgres.host` / `port` | `127.0.0.1` / `5432` | the server |
| `postgres.database` / `user` | `maillens` / `maillens` | |
| `postgres.password` | empty | or the environment variable `MAILLENS_PG_PASSWORD` |
| `postgres.sslmode` | `disable` | `disable`, `allow`, `prefer`, `require`, `verify-ca`, `verify-full` |
| `postgres.schema` | `maillens` | all tables live in this schema |
| `outlook.stores` | `["mailbox", "pst"]` | what to index |
| `outlook.exclude_folders` | `[]` | folder names, or paths below the store (`Inbox\Newsletters`), left out with everything below them |
| `outlook.start_if_needed` | `false` | start Outlook when it is not running |
| `sync.interval_minutes` | `10` | between two syncs of `run` |
| `sync.pause_ms` | `0` | pause after every message read |
| `sync.body_max_chars` | `2000000` | longer bodies are cut |
| `sync.batch_size` | `50` | messages per database write |
| `sync.mirror_deletions` | `true` | remove from the index what is gone from Outlook |
| `sync.mass_delete_percent` / `mass_delete_min` | `10` / `1000` | the deletion safeguard |
| `web.listen` | `127.0.0.1:8765` | must be a loopback address |
| `web.page_size` | `50` | results per page |
| `search.max_candidates` | `5000` | matches ranked by relevance |
| `log.dir` / `log.level` | `logs` / `info` | one file per day; `debug`, `info`, `warn`, `error` |

## Troubleshooting

| Symptom | Cause and remedy |
|---|---|
| `cannot connect to Outlook (HRESULT 0x800401E3)` | Outlook is not running, or runs at another privilege level. Start Outlook, and start maillens from a normal (non-administrator) terminal, the way you start Outlook. |
| `check` hangs at "reading a message" | Outlook shows a security prompt (programmatic access). Answer it, or see File > Options > Trust Center > Trust Center Settings > Programmatic Access. |
| `the password is wrong or missing` | Set `postgres.password` or `MAILLENS_PG_PASSWORD`. |
| `the database does not exist` | Create it (Setup, step 1). |
| `another sync is already running` | `maillens run` or another `maillens sync` is working on the same database. |
| `refusing to remove this many messages` | A store or folder may be missing from Outlook (a detached `.pst`?). Check with `maillens check -v`; if the removal is intended: `maillens sync -allow-mass-delete`. |
| no result for a word | Words such as "the" or "after" are ignored; try another word, or sort by newest first when the page says the ranking was cut. |
| `N folder(s) could not be scanned` | See the log (`logs\maillens-YYYYMMDD.log`); nothing was deleted from that store in that run. |

## Limits

Classic Outlook only; the primary mailbox and attached `.pst` files only; English text search (other languages are stored but neither stemmed nor, for Chinese and Japanese, split into words); attachment contents and meeting requests are not indexed; "Open in Outlook" is a best-effort link. Full list: `docs/design.md`.

## Development

```powershell
.\scripts\build.ps1                       # go vet, go test, go build
go test ./...                             # the PostgreSQL tests are skipped without MAILLENS_TEST_PG
$env:MAILLENS_TEST_PG = 'postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable'
go test ./internal/store/pg/              # needs a SCRATCH PostgreSQL: it creates and drops the schema maillens_it
```

Layout: `cmd/maillens` (commands), `internal/source` (the interface to Outlook) and `internal/source/outlook` (COM), `internal/engine` (sync), `internal/store` and `internal/store/pg` (PostgreSQL), `internal/web` (search page), `internal/config`, `internal/logx`, `internal/model`, `internal/version`. Rules for changes: `AGENTS.md`. Documents: `docs/README.md`.

---

# 中文

# MailLens

在毫秒内搜索一个很大的 Outlook 邮箱。MailLens 把本机 Outlook 里邮件的文本复制到 PostgreSQL，让这份副本始终与邮箱一致，并在 PostgreSQL 全文检索之上提供搜索页和命令行。邮件本身仍留在 Outlook；一切都留在你的电脑上。

当前版本：**0.1.0**（见 `VERSION`）。

## 状态：哪些验证过，哪些没有

- **已验证（2026-11-23）：**
  - 配置、Outlook 行转换、同步逻辑（用假 Outlook 和假数据库：新邮件、有变化的、被移动的、被删除的和读不了的邮件，被中断的运行，不完整的扫描，删除保护）以及 Web 接口的单元测试；
  - 对真实 PostgreSQL 16.4（一次性的临时服务器）的 10 个集成测试：表结构、排序、过滤、摘要、分页、移动、删除、锁、特殊文本；
  - 对该服务器端到端运行 `bin\maillens.exe`（40 项检查）：各命令、错误提示、搜索页及其接口，以及 Outlook 连不上时 `maillens run` 仍保持运行。
- **未验证：COM 适配层对真实 Outlook 的读取**（`internal/source/outlook`，也就是读取邮件的那一部分）。它是根据 Outlook 对象模型的文档和 `go-ole` 的源码写的，而构建它的会话是以管理员身份运行的，Outlook 的 COM 接口会拒绝这种情况。所以请在普通终端里先运行 `maillens check`，再运行 `maillens sync -limit 200`，并查看日志：预计要在这里修正一些细节。在 50 GB 邮箱上的速度还没有测量过。

## 要求

- Windows，安装并运行着**经典版** Outlook（Microsoft 365 应用，或 Outlook 2016 及更新版本）。"新版 Outlook"没有 COM 接口，无法读取。
- 启动 maillens 的**权限级别必须与 Outlook 相同**：用普通终端，不要"以管理员身份运行"。
- PostgreSQL 12 或更新（测试用的是 16.4），以及一个数据库和一个能在其中创建 schema 的用户。
- 构建需要 Go 1.27。

## 安装与使用

1. 创建数据库（只需一次；用 pgAdmin，或 PostgreSQL 的 `bin` 文件夹里的 `psql`）：

   ```sql
   CREATE ROLE maillens LOGIN PASSWORD '<自己选一个密码>';
   CREATE DATABASE maillens OWNER maillens;
   ```

2. 配置：把 `config\maillens.example.json` 复制为 `config\maillens.json` 并修改。密码写在 `postgres.password`，或放在环境变量 `MAILLENS_PG_PASSWORD` 里。该文件被 git 忽略。
3. 构建：`.\scripts\build.ps1`（先运行 `go vet` 和 `go test`，再构建 `bin\maillens.exe`）。
4. 在**普通**终端里，Outlook 正在运行时：`bin\maillens.exe check`。加 `-v` 会列出将被索引的存储和文件夹。它只输出数量和耗时，绝不输出邮件文本。
5. 先试一下：`bin\maillens.exe sync -limit 200`，然后 `bin\maillens.exe search <词>`。
6. 完整同步：`bin\maillens.exe sync`。大邮箱要花几个小时。可以按 Ctrl+C 停下，之后继续（会接着处理缺少的部分）；最新的邮件在最前面；`sync.pause_ms` 可以放慢速度，让 Outlook 保持流畅。
7. 日常使用：`bin\maillens.exe run` 提供搜索页，并每隔 `sync.interval_minutes` 同步一次。要在登录时启动，请在任务计划程序里创建任务：触发器选"登录时"，选"只在用户登录时运行"，并且**不要**勾选"使用最高权限运行"。

打开 `http://127.0.0.1:8765/`（或 `web.listen` 里的地址）。

## 命令

| 命令 | 作用 |
|---|---|
| `check [-v]` | 测试配置、PostgreSQL 以及与 Outlook 的连接（读取几行和一封邮件来计时） |
| `migrate` | 创建或更新表（`sync`、`serve`、`run` 也会执行） |
| `sync` | 把邮件从 Outlook 复制到 PostgreSQL；参数：`-limit N`、`-store 文字`、`-folder 文字`、`-retry-failed`、`-dry-run`、`-allow-mass-delete` |
| `serve` | 提供搜索页（不需要 Outlook） |
| `run` | `serve` 加上每隔 `sync.interval_minutes` 同步一次，直到按 Ctrl+C；Outlook 连不上时页面仍然可用，同步会稍后重试 |
| `search <查询>` | 在命令行搜索；参数：`-from`、`-since`、`-until`、`-folder`、`-attach`、`-sort relevance\|date`、`-n`、`-page` |
| `status` | 索引里有什么，以及上次同步的情况 |
| `version` | 打印版本 |

每个命令都接受 `-config <文件>`（默认：程序旁边或上一级文件夹里的 `config\maillens.json`）。

## 搜索

多个词按 AND 组合；支持 `"精确短语"`、`a OR b` 和 `-排除词`。搜索是英文的：词会还原为词干（attached、attaching、attach），"the"、"after" 这类很常见的词会被忽略，所以只由它们组成的查询什么也找不到。型号、批号、错误代码都是普通的词；尖括号里的文字（`<ABC123>`）也能搜到。搜索范围包括发件人、主题、收件人、附件名和正文；主题的权重最大。过滤：发件人、收到日期的起止、文件夹（含其下所有文件夹）、仅含附件的邮件。排序：按相关度（对最新的 `search.max_candidates` 个匹配排序）或最新的在前。

## 索引哪些内容

- 主 Exchange 邮箱，以及挂载到 Outlook 的 `.pst` 文件。共享邮箱、联机存档、公共文件夹和 IMAP 缓存不索引。没有挂载到 Outlook 的 `.pst` 读不了：请在 Outlook 里挂载（文件 > 打开和导出 > 打开 Outlook 数据文件）。
- 所有邮件文件夹，除了已删除邮件、垃圾邮件、草稿、发件箱、系统文件夹（同步问题、RSS 源、对话历史之类）以及 `outlook.exclude_folders` 里的文件夹。日历、联系人和任务不是邮件，会被跳过。
- 每封邮件：文件夹、发件人、收件人（按 Outlook 显示的）、日期、大小、重要性、附件**名称**，以及纯文本正文（超过 `sync.body_max_chars` 的部分会被截断）。附件内容不会复制，也不索引。

## 让索引与邮箱保持一致

在 Outlook 中消失的邮件会从索引中删除（`sync.mirror_deletions`，默认开启）。因为"存储不见了"与"邮件被删除"看起来完全一样，所以设置了保护措施：扫描不完整的存储不会删除任何东西；一次运行如果要删除超过 `sync.mass_delete_min` 封并且超过该存储 `sync.mass_delete_percent` 的邮件，会被拒绝（`maillens sync -allow-mass-delete` 可强制执行）。把邮件移到另一个文件夹会被识别出来，不会重新读取；阅读或标记邮件也不会触发重新读取。关闭 `mirror_deletions` 后永远不会删除任何东西，索引就成了存档。详见 `docs/design.md`。

## 隐私与安全

- 任何内容都不离开你的电脑。maillens 除了连接你的 PostgreSQL，不建立任何网络连接。
- 搜索页只监听回环地址（`web.listen` 的其他取值会被拒绝），只应答发往回环名称的请求，只读，并且不设置 CORS 头。
- 日志和错误信息里只有数量、文件夹路径和错误码，绝不包含主题、正文或地址。
- **数据库里是你全部邮件的明文。** 要像保护邮箱一样保护数据库及其备份（磁盘加密、访问权限）。如果你的备份脚本覆盖了数据库所在的文件夹，请决定索引是否应该被备份。

## 配置（`config\maillens.json`）

省略的键保持默认值；未知的键会报错。

| 键 | 默认值 | 含义 |
|---|---|---|
| `postgres.host` / `port` | `127.0.0.1` / `5432` | 服务器 |
| `postgres.database` / `user` | `maillens` / `maillens` | |
| `postgres.password` | 空 | 或使用环境变量 `MAILLENS_PG_PASSWORD` |
| `postgres.sslmode` | `disable` | `disable`、`allow`、`prefer`、`require`、`verify-ca`、`verify-full` |
| `postgres.schema` | `maillens` | 所有表都在这个 schema 里 |
| `outlook.stores` | `["mailbox", "pst"]` | 索引什么 |
| `outlook.exclude_folders` | `[]` | 文件夹名，或存储之下的路径（`Inbox\Newsletters`），连同其下所有内容一起排除 |
| `outlook.start_if_needed` | `false` | Outlook 没在运行时启动它 |
| `sync.interval_minutes` | `10` | `run` 两次同步之间的间隔 |
| `sync.pause_ms` | `0` | 每读一封邮件后暂停多久 |
| `sync.body_max_chars` | `2000000` | 更长的正文会被截断 |
| `sync.batch_size` | `50` | 每次写数据库的邮件数 |
| `sync.mirror_deletions` | `true` | 把在 Outlook 中消失的邮件从索引中删除 |
| `sync.mass_delete_percent` / `mass_delete_min` | `10` / `1000` | 删除保护 |
| `web.listen` | `127.0.0.1:8765` | 必须是回环地址 |
| `web.page_size` | `50` | 每页结果数 |
| `search.max_candidates` | `5000` | 按相关度排序的匹配数 |
| `log.dir` / `log.level` | `logs` / `info` | 每天一个文件；`debug`、`info`、`warn`、`error` |

## 故障排除

| 现象 | 原因与处理 |
|---|---|
| `cannot connect to Outlook (HRESULT 0x800401E3)` | Outlook 没在运行，或运行在另一个权限级别。先启动 Outlook，再从普通（非管理员）终端启动 maillens，方式与启动 Outlook 一样。 |
| `check` 卡在 "reading a message" | Outlook 弹出了安全提示（编程访问）。请回答它，或查看 文件 > 选项 > 信任中心 > 信任中心设置 > 编程访问。 |
| `the password is wrong or missing` | 设置 `postgres.password` 或 `MAILLENS_PG_PASSWORD`。 |
| `the database does not exist` | 创建它（安装与使用，第 1 步）。 |
| `another sync is already running` | `maillens run` 或另一个 `maillens sync` 正在处理同一个数据库。 |
| `refusing to remove this many messages` | 可能是 Outlook 里缺了某个存储或文件夹（`.pst` 被卸载了？）。用 `maillens check -v` 检查；如果确实要删除：`maillens sync -allow-mass-delete`。 |
| 某个词没有结果 | "the"、"after" 这类词会被忽略；换一个词，或者在页面提示排序被截断时改为按最新排序。 |
| `N folder(s) could not be scanned` | 查看日志（`logs\maillens-YYYYMMDD.log`）；该次运行没有从那个存储删除任何东西。 |

## 限制

只支持经典版 Outlook；只支持主邮箱和已挂载的 `.pst` 文件；英文全文检索（其他语言会保存，但不做词干化，中日文也不会切分成词）；不索引附件内容和会议请求；"在 Outlook 中打开"是尽力而为的链接。完整列表见 `docs/design.md`。

## 开发

```powershell
.\scripts\build.ps1                       # go vet、go test、go build
go test ./...                             # 没有 MAILLENS_TEST_PG 时跳过 PostgreSQL 测试
$env:MAILLENS_TEST_PG = 'postgres://postgres@127.0.0.1:55439/postgres?sslmode=disable'
go test ./internal/store/pg/              # 需要一个临时的 PostgreSQL：它会创建并删除 schema maillens_it
```

目录结构：`cmd/maillens`（命令）、`internal/source`（与 Outlook 的接口）和 `internal/source/outlook`（COM）、`internal/engine`（同步）、`internal/store` 和 `internal/store/pg`（PostgreSQL）、`internal/web`（搜索页）、`internal/config`、`internal/logx`、`internal/model`、`internal/version`。修改规则见 `AGENTS.md`。文档见 `docs/README.md`。
