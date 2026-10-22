# AGENTS.md — free2api

> 项目记忆（最小版）。先读本文件，再按任务加载必要 skill；不要为了“守规矩”预加载整套 skill 链。

## [项目家风 - 状态优先行动协议]

- 先只读侦察，再下结论、改代码、重启服务。
- 写/删/重启前先确认：影响范围、可逆性、当前是否正通过网关承流。
- 发现文档与代码冲突时，以代码、测试、实际运行结果为准，并把冲突留在对话里确认，不自行脑补。

## [已生效]

### 项目定位

- free2api 是单进程、可多 listener 的本地 AI 反代网关，把 WorkBuddy / ZCode / OpenCode / Kilo 等来源的账号与模型统一成 OpenAI Chat Completions 和 OpenAI Responses 两种出口。
- 非官方项目；README 明确限定为本人授权账号、本机或私有环境测试。
- 主要实现是 Go（`go 1.22.5`），无数据库；持久状态落在 JSON 文件。

### 入口与目录

- `cmd/server`：命令行网关入口，最终装配在 `internal/gateway`。
- `cmd/desktop`：Windows WebView2 桌面控制台；与 `cmd/server` 共用 `internal/gateway` 装配。
- `cmd/attach`：Codex 接入/还原；`cmd/login`、`cmd/signin`、`cmd/acct`、`cmd/credit` 等是账号辅助工具。
- `internal/source`：本机登录态取源；`internal/pool`：选号/冷却/熔断/粘性；`internal/upstream`：上游协议与出站；`internal/server`：本地 API、模型目录、发布清单、统计与多出口；`internal/webui`：内嵌管理页。
- `cmd/server` 与 `cmd/desktop` 都内嵌 `internal/webui/index.html`。改了管理页后，发布的两个二进制都要重新构建。

### 运行与构建

- 源码检查：`go build ./...`、`go vet ./...`、`go test ./...`。
- 命令行网关：`go run ./cmd/server -config config.json`。
- 构建服务端：`go build -trimpath -ldflags="-s -w" -o free2api-server.exe ./cmd/server`。
- 构建桌面端：`go build -trimpath -ldflags="-s -w -H=windowsgui" -o free2api.exe ./cmd/desktop`。
- 发布前至少跑受影响包的测试；涉及桌面生命周期、端口或 WebView2 时，必须跑 `go test ./cmd/desktop`。

### 运行目录与持久化

- `config.json` 是启动配置（含密钥，禁止提交）；`auths/` 是账号凭证（禁止提交）。
- `data/state.json` 是号池运行态；`data/output.json` 是运行期输出参数与多出口；`data/model.json` 是模型能力缓存；`data/zcode_entitled.json` 是 ZCode 权益快照。
- `data/stats.json` 是统计持久化；`data/server.*.log` 与 `data/desktop.log` 是运行日志。
- 迁移一个完整部署时，至少带走 `config.json`、`auths/`、`data/`；只带 exe 会丢账号和状态。
- 部署目录固定：升级只覆盖同一个部署目录里的 exe，不按版本另开目录。桌面程序在
  exe 同目录没有配置时会按「最近使用」接手本盘 / 用户目录里的已有部署
  （见 `cmd/desktop/deploydir_windows.go`），但运行态仍以 `config.json` /
  `auths/` / `data/` 所在目录为准；备份目录名（*bak*、*backup*、*old*、
  `dist-v*`、日期目录）不会被当成当前部署。

### 模型路由与对外命名

- 入站模型名协议：`[realm:][producer:]model`。`realm` 当前是 `cn` / `global`，缺省为 `cn`；`producer` 是 `workbuddy` / `zcode` / `qoder` / `opencode` / `kilo`。
- 网关对外模型 id = 来源/模型裸名 + 可配置的模型前缀 + 费率后缀。入站必须对称剥掉输出前缀与费率后缀后再匹配裸名。
- 同一裸模型名可能存在于多个来源；显式 `producer:` 前缀用于钉死来源，否则由目录归属推断。
- `/v1/chat/completions` 是 OpenAI 兼容出口；`/v1/responses` 是 Codex Responses 出口；两者共用同一套号池与上游语义。
- 多出口是同一进程内的多个 listener，共享号池、粘性会话、成本与冷却状态；不要改成每口一个进程，否则会重复选号。

### 网关生命周期

- 桌面程序启动时接管网关；停止网关是全局语义，会停托管实例、清理同目录残留进程并暂停看门狗。
- 端口控制权不是“谁先 bind 谁算”，还有命名互斥体租约；停止时先完成清理，再释放租约。
- WebView2 只访问本机控制服务。必须保持强制直连参数，避免系统代理/VPN 把 `127.0.0.1` 截走。
- 桌面端关闭窗口等于退出并清理网关；不要留下“窗口没了但旧进程仍占端口”的尾巴。

### 高风险改动

- 修改模型前缀、费率后缀、发布清单、realm/producer 选择逻辑时，必须同时检查对外拼名与入站剥名，并补回归测试。
- 修改号池并发、冷却、熔断、粘性或过期积分窗口时，必须检查单账号并发上限与多出口共享状态。
- 修改 `internal/webui/index.html` 时，同时检查嵌入式页面测试与两个二进制的重建。
- 修改 WebView2、悬浮窗、端口启停、看门狗时，按桌面生命周期回归；不能只看网页版。
- 需要重启当前正在承流的网关时，先准备好独立的重启动作，再停旧进程；不要在同一连接里“先关再拉”。

## [待确认]

- [桌面发布]：Windows 桌面端发布必须使用 `-H=windowsgui`；manifest、图标和运行时 FreeConsole 都不能把 PE 子系统从 Console 改成 GUI。（证据：已核查部署 exe 的 PE Subsystem=2；`cmd/desktop/desktop_test.go` 的 TestWindowsDesktopBuildUsesGUISubsystem 防止 README/workflow 回退；使用条件：仅用于 cmd/desktop 的 Windows 发布构建，cmd/server 仍保留控制台子系统。）

## [安全红线 - 不可触碰区域]

- 不提交 `config.json`、`auths/`、`data/`、API key、cookie、token、日志原文或用户账号信息。
- 不批量删除 `auths/`、`data/`、`config.json`；清理必须先列出目标并确认不破坏运行态。
- 不自动执行不可逆的上游账号操作、积分领取、删除账号或高风险批量请求。
- 不在没有回滚方案时重写持久状态文件或迁移账号目录。

## Bootstrap 状态

- `[已生效] bootstrap: done @ 2026-10-18`
- 依据：README、`go.mod`、`cmd/`、`internal/`、测试文件与当前运行配置；未核实的经验不写成事实。
