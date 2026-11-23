# Project Engineering Instructions

These instructions apply to all AI-assisted development in this repository.

本说明适用于本代码仓库中的所有 AI 辅助开发工作。

---

# English

## 1. General Principles

Prefer correctness, maintainability and clarity over clever solutions. Keep changes focused on the requested task. Reuse the existing structure: Outlook is reached only through `internal/source`, the database only through `internal/store`, so the logic stays testable with fakes.

Do not claim that a task, test or verification was completed unless it was actually performed. If something could not be verified, say what and why.

## 2. Mail Data Is Private

The index holds the text of all of the user's mail.

- Nothing leaves the machine. Do not add network calls, telemetry, cloud services or any export of mail text.
- The web page only listens on a loopback address (`config.checkLoopback`) and only reads. Do not weaken that, and do not add CORS headers.
- Never write subjects, bodies, addresses or attachment names to logs, error messages or test output. Counts, folder paths and error codes are fine.
- Tests use invented mail only. Never put real mail into the repository, a test, a document or a commit message.

## 3. Configuration and Secrets

Do not hard-code credentials, hosts, ports, paths or addresses. Settings come from `config\maillens.json` (git-ignored; only `config\maillens.example.json` is tracked). The database password may be given in the environment variable `MAILLENS_PG_PASSWORD`. Unknown keys in the config file are an error, not ignored.

## 4. Database

- Use parameterised queries. Only the schema name is put into a statement, and only after it matched `^[a-z_][a-z0-9_]{0,62}$` and was quoted (`DB.q`).
- Schema changes are new entries at the end of `migrations` in `internal/store/pg/migrate.go`; never edit an entry that was released.
- Do not run destructive statements against a database that is not a scratch database. The integration tests create and drop the schema `maillens_it`; point `MAILLENS_TEST_PG` only at a scratch PostgreSQL.
- Deleting from the index is guarded (section 6 of `docs/design.md`): keep the safeguards when changing the sync.

## 5. Testing and Verification

Verify in proportion to the risk: unit tests, then the PostgreSQL tests against a scratch server, then a manual run.

- The sync logic (`internal/engine`) is tested with a fake source and a fake database. Add a test for every rule you change, in particular for anything that can delete.
- The SQL is tested against a real PostgreSQL (`MAILLENS_TEST_PG`). A statement that was not run against a server is not verified.
- The COM adapter (`internal/source/outlook`) can only be exercised against a running Outlook at the same privilege level; the pure parts (`convert.go`) are unit tested. Say clearly what was run against a real Outlook and what was not.
- Do not modify or weaken a test to hide a real failure.

## 6. Dependencies

Prefer the standard library. The dependencies are `pgx` (PostgreSQL), `go-ole` (COM) and `golang.org/x/sys`. Do not add another one when these or the standard library can do the job.

## 7. Documentation Language Requirements

All documentation created or modified by AI agents MUST be bilingual: the complete English version first, then the complete Simplified Chinese version, with the same structure and the same technical content. Identifiers (file names, commands, configuration keys, function names) stay unchanged in both languages. Do not duplicate identical code blocks for translation.

This does not apply to source code, identifiers, log and error messages, inline comments, or Git commit messages (these are English).

## 8. Documentation Quality

Describe the actual implementation, not an assumed one. Do not document a feature as done if it is only planned, and do not invent keys, commands or behaviour. Keep `docs/README.md` (the index) up to date when adding a document.

## 9. Completion

A task is not complete because code was written. Before finishing: tests pass, the build works (`.\scripts\build.ps1`), temporary files are gone, the documentation matches the code, the version is bumped where needed (`VERSION` and `internal/version/version.go`), and open problems are reported. Commit messages are one line: `vX.Y.Z -- summary`.

## 10. Known Pitfalls

### 10.1 Outlook COM only works at the same privilege level as Outlook

`GetActiveObject("Outlook.Application")` fails with `0x800401E3` (MK_E_UNAVAILABLE), and creating the object with `0x80080005` (CO_E_SERVER_EXEC_FAILURE), when the caller is elevated and Outlook is not (or the other way round), and also when Outlook is simply not running. A caller that dropped admin rights through a restricted token still has the High integrity level, so it fails the same way.

Rule: run maillens from a normal, non-administrator terminal, started the way Outlook is started. `internal/source/outlook` explains this in the error. Do not try to get around the privilege separation; ask the user to start the program at the right level.

### 10.2 A tsvector cannot be larger than 1 MB

A long mail body made the insert fail with "string is too long for tsvector". The generated column indexes only `left(body, 400000)`; the whole body is still stored and shown. Covered by `TestTextThatCouldBreakTheInsert` (a 3 MB body).

### 10.3 Words such as "the" or "after" are not searchable

The `english` text search configuration drops stop words, so a query made only of them is empty and finds nothing. This is not a bug; the page and the command line say so. Use ordinary words in tests (a test that searched for "after" failed for this reason).

### 10.4 The whole database target goes into ONE connection string

`config.Postgres.DSN()` builds the whole target (host, port, user, password, database, timeouts) as one string and the driver parses that. Never patch `Host` or `Port` of a parsed configuration: its fallback list keeps the defaults and the driver silently tries `localhost:5432` (found in the sister project `dash_go`). `TestDSNSurvivesSpecialCharactersAndTheDriverParser` covers it. Errors that may contain the connection string go through `scrub`.

### 10.5 A PostgreSQL started from PowerShell with a redirect never lets go

`& pg_ctl start *> file` (and `Start-Process -Wait`) makes PowerShell wait until the server, which inherited the output handles, exits. For a scratch server in a test script: start `pg_ctl` with `Start-Process -WindowStyle Hidden` without redirect or wait, wait for `pg_isready`, and stop it with `pg_ctl stop` in a `finally` block. From bash, redirecting to files works. PostgreSQL refuses to run as administrator, but `initdb` and `pg_ctl` drop the rights themselves.

### 10.6 Outlook's bulk table

`Folder.GetTable` returns metadata fast but cuts long values such as the body, so bodies are read one item at a time. Its date values are in UTC (`MailItem` properties are local time). Rows are read by column position (`Row.Item(i)`, 1 based) in the order of `tableColumns`; changing a column means changing the constant `col*` list too. An item moved to another folder gets a new entry id, which is why the sync matches moves by Message-ID and size instead of reading the item again.

### 10.7 PostgreSQL's parser drops what is between angle brackets

`to_tsvector` reads `<ABC123>` as an HTML tag and drops it, and drops the text between `<script>` and `</script>` as well. Mail often puts part numbers, lot numbers and error codes between brackets (`Lot <ABC123> failed`, `<<<ERROR>>>`), so they were not searchable. The search column and the snippet therefore run the text through `translate(text, '<>', '  ')`; the stored body keeps its brackets. Covered by `TestIdentifiersInAngleBracketsAreSearchable`. Addresses (`<jane@example.com>`) and web addresses survive either way.

---

# 中文

## 1. 通用原则

优先考虑正确性、可维护性和清晰度，不追求炫技。修改范围聚焦于当前任务。复用现有结构：只通过 `internal/source` 访问 Outlook，只通过 `internal/store` 访问数据库，这样逻辑才能用假对象测试。

除非实际执行过，否则不得声称任务、测试或验证已经完成。无法验证的内容要说明是什么、为什么。

## 2. 邮件数据属于隐私

索引里有用户全部邮件的正文。

- 任何内容都不得离开本机。不要加入网络调用、遥测、云服务，也不要导出邮件文本。
- Web 页只监听回环地址（`config.checkLoopback`），并且只读。不要削弱这一点，也不要加 CORS 头。
- 不要把主题、正文、地址或附件名写进日志、错误信息或测试输出。数量、文件夹路径和错误码可以写。
- 测试只用虚构的邮件。不要把真实邮件放进仓库、测试、文档或提交信息。

## 3. 配置与机密

不要硬编码凭据、主机、端口、路径或地址。设置来自 `config\maillens.json`（被 git 忽略，只跟踪 `config\maillens.example.json`）。数据库密码可以放在环境变量 `MAILLENS_PG_PASSWORD` 中。配置文件里的未知键是错误，不会被忽略。

## 4. 数据库

- 使用参数化查询。只有 schema 名会被放进语句，并且必须先匹配 `^[a-z_][a-z0-9_]{0,62}$` 并加引号（`DB.q`）。
- 表结构变更是在 `internal/store/pg/migrate.go` 的 `migrations` 末尾追加新条目；不要修改已发布的条目。
- 不要对非临时库执行破坏性语句。集成测试会创建并删除 schema `maillens_it`；`MAILLENS_TEST_PG` 只能指向临时的 PostgreSQL。
- 从索引里删除是有保护的（`docs/design.md` 第 6 节）：修改同步逻辑时要保留这些保护。

## 5. 测试与验证

按风险程度验证：先单元测试，再对临时服务器跑 PostgreSQL 测试，最后手工运行。

- 同步逻辑（`internal/engine`）用假数据源和假数据库测试。修改任何规则都要加测试，尤其是可能删除数据的部分。
- SQL 要对真实的 PostgreSQL 测试（`MAILLENS_TEST_PG`）。没有在服务器上运行过的语句不算已验证。
- COM 适配层（`internal/source/outlook`）只能在与 Outlook 相同权限级别、且 Outlook 正在运行时才能实际运行；纯逻辑部分（`convert.go`）有单元测试。要明确说明哪些对真实 Outlook 运行过，哪些没有。
- 不要为了掩盖真实失败而修改或削弱测试。

## 6. 依赖

优先使用标准库。依赖只有 `pgx`（PostgreSQL）、`go-ole`（COM）和 `golang.org/x/sys`。这些和标准库能完成的事，不要再加新依赖。

## 7. 文档语言要求

所有由 AI Agent 创建或修改的文档都必须是双语：先是完整的英文版，再是完整的简体中文版，结构和技术内容相同。标识符（文件名、命令、配置键、函数名）在两种语言中保持不变。不要仅为翻译而重复相同的代码块。

此要求不适用于源代码、标识符、日志和错误信息、行内注释以及 Git 提交信息（这些使用英文）。

## 8. 文档质量

描述真实的实现，而不是假设的实现。不要把仅在计划中的功能写成已完成，也不要虚构配置键、命令或行为。新增文档时同步更新 `docs/README.md`（索引）。

## 9. 完成标准

写完代码不等于完成。结束前：测试通过，构建可用（`.\scripts\build.ps1`），临时文件已清理，文档与代码一致，需要时已升版本号（`VERSION` 和 `internal/version/version.go`），并报告未解决的问题。提交信息是一行：`vX.Y.Z -- 摘要`。

## 10. 已知问题与经验

### 10.1 Outlook 的 COM 只有在与 Outlook 相同的权限级别时才能用

调用方是提权的而 Outlook 不是（或相反），以及 Outlook 根本没在运行时，`GetActiveObject("Outlook.Application")` 会失败并返回 `0x800401E3`（MK_E_UNAVAILABLE），创建对象则返回 `0x80080005`（CO_E_SERVER_EXEC_FAILURE）。通过受限令牌去掉管理员权限的进程，完整性级别仍是 High，同样会失败。

规则：从普通的、非管理员的终端运行 maillens，启动方式与启动 Outlook 的方式相同。`internal/source/outlook` 会在错误信息里说明这一点。不要试图绕过权限隔离；请用户以正确的级别启动程序。

### 10.2 tsvector 不能超过 1 MB

很长的邮件正文曾让插入失败，报错 "string is too long for tsvector"。生成列只对 `left(body, 400000)` 建索引；完整正文仍然会保存并显示。由 `TestTextThatCouldBreakTheInsert`（3 MB 的正文）覆盖。

### 10.3 "the"、"after" 这类词搜不到

`english` 全文检索配置会丢掉停用词，所以只由这些词组成的查询是空的，什么也找不到。这不是缺陷；网页和命令行都会提示。测试里要用普通的词（曾有一个搜 "after" 的测试因此失败）。

### 10.4 数据库的完整目标要放进同一个连接字符串

`config.Postgres.DSN()` 把完整目标（主机、端口、用户、密码、数据库、超时）构造成一个字符串，由驱动解析。绝不要修改已解析配置的 `Host` 或 `Port`：它的 fallback 列表仍是默认值，驱动会悄悄尝试 `localhost:5432`（在姊妹项目 `dash_go` 中发现）。由 `TestDSNSurvivesSpecialCharactersAndTheDriverParser` 覆盖。可能包含连接字符串的错误要经过 `scrub`。

### 10.5 在 PowerShell 里带重定向启动 PostgreSQL，会一直等下去

`& pg_ctl start *> file`（以及 `Start-Process -Wait`）会让 PowerShell 一直等到服务器退出，因为服务器继承了输出句柄。测试脚本里的临时服务器：用 `Start-Process -WindowStyle Hidden` 启动 `pg_ctl`，不重定向也不等待，用 `pg_isready` 等待就绪，并在 `finally` 里用 `pg_ctl stop` 停止。在 bash 里重定向到文件则没有问题。PostgreSQL 拒绝以管理员身份运行，但 `initdb` 和 `pg_ctl` 会自己去掉权限。

### 10.6 Outlook 的批量表

`Folder.GetTable` 读元数据很快，但会截断正文这类长值，所以正文只能逐封读取。它的日期值是 UTC（`MailItem` 的属性是本地时间）。行按列位置读取（`Row.Item(i)`，从 1 开始），顺序与 `tableColumns` 一致；改一列就要同时改 `col*` 常量。邮件移到另一个文件夹后会得到新的 entry id，所以同步按 Message-ID 和大小识别"移动"，而不是重新读取。

### 10.7 PostgreSQL 的解析器会丢掉尖括号之间的内容

`to_tsvector` 会把 `<ABC123>` 当成 HTML 标签丢掉，`<script>` 和 `</script>` 之间的文字也会丢掉。邮件里常把型号、批号和错误代码放在尖括号里（`Lot <ABC123> failed`、`<<<ERROR>>>`），所以它们原先搜不到。因此搜索列和摘要都会先对文本执行 `translate(text, '<>', '  ')`；保存的正文仍保留尖括号。由 `TestIdentifiersInAngleBracketsAreSearchable` 覆盖。邮箱地址（`<jane@example.com>`）和网址不受影响。
