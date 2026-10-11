# qwen code 接入说明

Qwen Code 是阿里开源的终端 AI 编程助手（npm 包 `@qwen-code/qwen-code`）。启动时会自动扫描项目的 Skill 目录并挂到会话上；对程序接入提供三种方式：交互模式、一次性命令、本地 HTTP 服务。

背景：原 trae-cli app-server 方案要求企业版账号，本项目改用 Qwen Code。实测版本：`qwen 0.25.1`（2026-10-11）。

## 安装

前置：Node.js >= 22。

```bash
npm install -g @qwen-code/qwen-code@latest
```

国内网络加镜像提速：

```bash
npm install -g @qwen-code/qwen-code@latest --registry=https://registry.npmmirror.com
```

验证：

```bash
qwen --version    # 应输出 0.25.1
```

## 登录

```bash
qwen      # 进入交互模式
/auth     # 在提示符里输入，选择认证方式
```

两种方式二选一：

- **Qwen OAuth**：浏览器登录 qwen.ai 账号，有免费额度；
- **百炼 API Key**：阿里云百炼控制台创建后粘贴，按量或套餐计费。

说明：`qwen auth` 子命令官方已移除，只能在交互模式里用 `/auth`；凭证保存在 `~/.qwen/oauth_creds.json`。

## Skill 自动识别

Qwen 会同时扫描项目下两个目录（源码常量 `PROJECT_SKILL_DIRS=[".qwen",".agents"]`）：

- `.qwen/skills/`
- `.agents/skills/`

全局目录为 `~/.qwen/skills/`。**无需任何启用参数**，启动时自动发现。

本仓库的 skill：`.agents/skills/zhangsan-eating/SKILL.md`，格式为 YAML frontmatter（`name`、`description`）+ Markdown 正文。

验证：

```bash
qwen -p "张三喜欢吃什么？" --output-format json
```

交互模式里提问会显示 `Use skill: "zhangsan-eating"`，并引用 SKILL.md 里的事实回答（红烧牛肉面）。

## 三种接入方式

| 方式 | 命令 | 适合什么 |
|---|---|---|
| 交互模式 | `qwen` | 人直接在终端里用 |
| 一次性（无头） | `qwen -p "问题" --output-format json` | 脚本/服务端每次调用，问完即退 |
| HTTP 服务 | `qwen serve` | 程序常驻接入，标准 HTTP 接口 |

### 一次性命令

```bash
qwen -p "张三喜欢吃什么？" --output-format json
```

每次执行都是新进程、新会话；`-p` 为非交互模式，`--output-format json` 输出结构化结果，便于脚本解析。

### HTTP 服务

```bash
qwen serve --no-web --port 4170 --workspace E:/go_ws/agent-cli-demo
```

参数要点：

- 默认绑定 `127.0.0.1:4170`；loopback 访问免鉴权。要强制鉴权用 `--require-auth` + `--token`（非 loopback 绑定会自动生成令牌）。
- `--no-web`：只开 API，不伺服根路径的 Web Shell 网页（默认开启）。
- `--workspace`：注册工作区绝对路径；`POST /session` 的 `cwd` 与注册值不一致会返回 400 `workspace_mismatch`。省略时默认用进程当前目录。
- 其它：`--port 0` 随机端口、`--max-sessions`（默认 32）、`--tls-cert/--tls-key` 启用 HTTPS。

#### 三步调用流程

程序接入是固定的三步：建会话 → 发问题 → 订阅事件流读回答。三条 curl 按顺序执行即可跑通（先做一次 `/auth` 登录）：

```bash
# 1. 建会话，从返回里拿 sessionId
curl -s -X POST http://127.0.0.1:4170/session \
  -H 'Content-Type: application/json' \
  -d '{"sessionScope":"thread"}'

# 2. 发问题，保留返回里的 promptId 和 lastEventId
curl -s -X POST http://127.0.0.1:4170/session/<sessionId>/prompt \
  -H 'Content-Type: application/json' \
  -d '{"prompt":[{"type":"text","text":"你的问题内容"}]}'

# 3. 订阅事件流；Last-Event-ID 用第 2 步返回的 lastEventId
curl -N http://127.0.0.1:4170/session/<sessionId>/events \
  -H 'Accept: text/event-stream' \
  -H 'Last-Event-ID: <lastEventId>'
```

说明：

- 第 2、3 步先后可以互换。先提问再订阅时，事件先落在服务端日志里，订阅时带 `Last-Event-ID` 会把缺的那段补发过来，不会丢；先订阅再提问也行，事件流本来就是实时推送的。
- 同一条会话里问第二句，只需重复第 2、3 步，`sessionId` 不变，模型能看到前面问过什么。
- 这就是"依次调三个接口"的由来：HTTP 本身不阻塞，还要自己解析 SSE 事件流、按 `promptId` 过滤、拼增量、等结束标记。用代码把三段流程包成一个接口（见下文 main.go），调用方就只剩一次 `POST /ask`。

#### 1. 健康检查

```http
GET /health
→ {"status":"ok"}
```

#### 2. 建会话

```http
POST /session
Content-Type: application/json

{"sessionScope":"thread"}
```

```json
{"sessionId":"...","workspaceCwd":"E:\\go_ws\\agent-cli-demo","attached":false,"clientId":"client_...","createdAt":"..."}
```

首次建会话要拉起工作区运行时，约 5 秒；之后很快。`sessionScope` 固定用 `"thread"` 即可；会话的工作目录由启动参数 `--workspace` 决定，skill 挂在会话的工作区上，所以 `--workspace` 必须是项目目录。

#### 3. 提问（异步）

```http
POST /session/<sessionId>/prompt
Content-Type: application/json

{"prompt":[{"type":"text","text":"张三最喜欢吃什么？"}]}
```

注意：`prompt` 必须是 content blocks 数组，直接给字符串会返回 400（`"prompt" is required and must be a non-empty array of content blocks`）。

返回 202 受理：

```json
{"promptId":"...","lastEventId":1,"eventEpoch":"..."}
```

`promptId` 用来在事件流里认出"这次提问"的事件；`lastEventId` 是提问前的游标，订阅事件流时带上它，就只会读到本次提问产生的部分。

#### 4. 取回答（SSE 事件流）

```http
GET /session/<sessionId>/events
Accept: text/event-stream
Last-Event-ID: <第 3 步返回的 lastEventId>
```

返回 `text/event-stream`：一条事件一段、空行分隔，响应头带 `X-Qwen-Event-Epoch`（与提问返回的 `eventEpoch` 对应）。事件类型：

| event | 含义 |
|---|---|
| `session_update` | 过程增量通知，回答片段（`agent_message_chunk`）在这里 |
| `turn_complete` | 本轮回答结束，`data.stopReason` 为 `end_turn` |
| `replay_complete` | 按 `Last-Event-ID` 补发历史结束后的标记 |
| `followup_suggestion` | 追问建议，可忽略 |

回答正文 = 所有 `update.sessionUpdate == "agent_message_chunk"` 事件的 `content.text` 按序拼接（是**增量片段**，不是整段）。两条真实事件（字段有省略）：

```text
id: 4
event: session_update
data: {"id":4,"v":1,"type":"session_update","data":{"sessionId":"<sessionId>","update":{"content":{"text":"Hello","type":"text"},"sessionUpdate":"agent_message_chunk"}},"promptId":"<promptId>","_meta":{"serverTimestamp":1791731147426}}

id: 9
event: turn_complete
data: {"id":9,"v":1,"type":"turn_complete","promptId":"<promptId>","data":{"sessionId":"<sessionId>","stopReason":"end_turn","promptId":"<promptId>"},"_meta":{"serverTimestamp":1791731147711}}
```

上面两条合起来回答就是 `Hello`；读到与本次 `promptId` 相同的 `turn_complete`，本次回答即结束。其它要点：

- 行格式：`event:` 行是类型、`data:` 行是 JSON、`id:` 行是自增事件 id；开头还有 `retry: 3000`，保活会周期发 `: heartbeat` 注释行，解析时都要忽略。
- 事件流不会自己断开，拿到 `turn_complete` 后由客户端关闭连接。
- 同一次提问的增量事件带同一个 `promptId`；子代理（如自动记忆）的增量文本为空且带 `subagentId`，拼正文时跳过。
- 注意：真实对话需要先完成 `/auth` 登录；未登录时提问会被受理（202），但拿不到回答内容。

#### 其它常用接口

- `GET /sessions`：会话列表（另有 `/sessions/archive`、`/sessions/unarchive`）
- `POST /session/<id>/cancel`：取消当前提问
- `GET /session/<id>/transcript`、`GET /session/<id>/export`：会话记录
- `POST /session/<id>/shell`：执行命令（需启动时加 `--enable-session-shell`）

## 项目里的 HTTP 封装

`main.go` 已经把上面的三步流程包成了 `POST /ask`：内部维护一条到 qwen serve 的会话（只建一次），每次提问自动完成「发问题 → 拿游标订阅事件流 → 拼增量 → 等到 `turn_complete`」，对外就是一个普通的一问一答 HTTP 接口，调用方不用碰 sessionId、SSE 和 promptId。

```bash
# 1. 先起 qwen serve：--workspace 必须是项目目录，skill 才挂得上
qwen serve --no-web --port 4170 --workspace E:/go_ws/agent-cli-demo

# 2. 再起封装服务
go run . -addr :8080
```

```http
POST http://127.0.0.1:8080/ask
Content-Type: application/json

{"question":"张三最喜欢吃什么？只回答食物名字。"}
```

```json
{"answer":"红烧牛肉面","duration_ms":4545,"source":"qwen-serve"}
```

参数：

- `-addr`：封装服务监听地址，默认 `:8080`
- `-qwen`：qwen serve 地址，默认 `http://127.0.0.1:4170`
- `-timeout`：单个问题超时秒数，默认 120

`GET /health` 返回连接状态、`sessionId` 和会话工作区。

内部行为要点：

- 会话懒创建：第一次 /ask 时建会话，之后所有问题复用同一个 `sessionId`；被服务端回收（闲置超时）后会自动重建并重试一次。
- 同一时间只处理一个提问（串行）；事件按 `promptId` 过滤，子代理事件（如自动记忆）不计入回答。
- 已实测跑通（qwen 0.25.1）：问「张三最喜欢吃什么？」返回「红烧牛肉面」；追问「他为什么喜欢？」能结合上下文回答，证明 skill 识别和会话复用都正常。

> 说明：`qwen serve` 官方标记为实验特性（Stage 1），接口可能随版本变化；本文命令均在 qwen 0.25.1 实测通过。
