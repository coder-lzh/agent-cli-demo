# trae-cli 与 Codex CLI 接入

两家给程序用的都是长连接上的 JSON-RPC，没有 qwen 那种单独的 `GET /events`。提问的回包只表示这一轮开始了，回答在同一条连接上以通知推过来。

本文只写问答主路径。trae-cli 的报文按本仓库实测的 `traecli 0.207.1`。Codex 的报文按官方 [Codex App Server](https://developers.openai.com/codex/app-server) 文档，字段会随 CLI 版本变，以本机 `codex app-server generate-json-schema` 为准。

方法名两边一样：`initialize`、`initialized`、`thread/start`、`turn/start`。连上之后的顺序也一样，差在沙箱字段的拼写，以及 Codex 还能在断线后用 `thread/resume` 把旧会话装回来。

## 一次性命令

不建连接、不发 JSON。每次都是新进程、新会话。

```bash
# trae-cli：-C 指向项目目录才会读到 .trae/skills；-y 不再弹出确认
trae-cli exec -C /Users/bytedance/ws/agent-cli-demo -y "张三最喜欢吃什么？只回答食物名字。"

# Codex CLI
codex exec "张三最喜欢吃什么？只回答食物名字。"
```

`trae-cli exec-server` 是另一套实验服务，用来跑进程和文件，不能拿来问 skill。本机如果两个端口都在，`ws://127.0.0.1:8766` 才是下面的 app-server。

## 三种接法

`app-server` 把同一套 JSON 送到三种传输上。stdio 是一行一个 JSON，以换行结尾。WebSocket 和 Unix socket 是一条文本帧一个 JSON。

| 接法 | trae-cli | Codex CLI | 适合什么 |
|---|---|---|---|
| stdio | `trae-cli app-server` | `codex app-server` | 父进程拉起，用标准输入输出对接 |
| Unix socket | `trae-cli app-server --listen unix://PATH` | `codex app-server --listen unix://PATH` | 本机多个程序共用一个常驻进程 |
| WebSocket | `trae-cli app-server --listen ws://127.0.0.1:8766` | `codex app-server --listen ws://127.0.0.1:4500` | Postman 或另一台机器来测 |

不写 `--listen` 时两边默认都是 stdio。Codex 的 WebSocket 官方标成实验能力；非本机地址要另配鉴权。Codex 的 WebSocket 端口同时提供 `GET /readyz`、`GET /healthz`，那两个只表示进程活着，不是问答接口。

下面的示例都按 WebSocket 文本帧来写。换成 stdio 时，每条 JSON 末尾加一个换行即可，字段不用改。

## 报文怎么认

带 `id` 的是请求，要等带同一个 `id` 的 `result` 或 `error`。不带 `id`、只有 `method` 的是通知，服务端推过来，没有回包。

```json
{"id":"3","method":"turn/start","params":{"threadId":"thr_123","input":[{"type":"text","text":"张三最喜欢吃什么？"}]}}
```

```json
{"id":"3","result":{"turn":{"id":"turn_456","status":"inProgress","items":[]}}}
```

```json
{"method":"item/agentMessage/delta","params":{"turnId":"turn_456","delta":"红烧"}}
```

第三条没有 `id`。它不是上面那次 `turn/start` 的响应，是回答开始往外推了。

## 给程序接的服务

一条连接上，前三步只做一次。连接不断的话，后面每个问题只重复 `turn/start`。连接断了要重新握手。

### 1. initialize

告诉服务谁连上来了，并协商这条连接要用的能力。此时还没有会话，也不能在这一步提问。`clientInfo` 是客户端自己的名字，写进服务端日志。`capabilities.experimentalApi` 设为 `true` 才可以使用实验字段。

trae-cli：

```json
{"id":"1","method":"initialize","params":{"clientInfo":{"name":"postman","title":"postman","version":"0.1.0"},"capabilities":{"experimentalApi":true}}}
```

等 `"id":"1"` 的 `result`。回包里有运行环境，例如 `codexHome` 和操作系统。

Codex 还可以在这一步关掉不需要的通知。名字要完全匹配，不支持通配：

```json
{"id":1,"method":"initialize","params":{"clientInfo":{"name":"my_client","title":"My Client","version":"0.1.0"},"capabilities":{"experimentalApi":true,"optOutNotificationMethods":["item/agentMessage/delta"]}}}
```

没做完这一步就发后面的方法，服务会拒绝。同一条连接上 `initialize` 只能发一次。

### 2. initialized

通知服务：握手结果我已经收到，这条连接可以正式用了。它不是请求，所以没有 `id`，一般也没有回包。不发这条，后面的 `thread/start` 会被拒绝。

```json
{"method":"initialized"}
```

### 3. thread/start

开一个会话。工作目录、要不要人工确认、沙箱，都定在这一步。回包里的 `result.thread.id` 就是会话 id，后面每句提问都带着它。同一条连接会自动开始接收这个会话的 `turn/*`、`item/*` 通知，不用再发一个订阅请求。

trae-cli 把 skill 挂在这个会话的工作目录上，所以 `cwd` 必须是项目目录，才能读到 `.trae/skills`。`ephemeral: true` 表示这个会话不落盘，连接结束后不留成历史。`sandbox` 的取值是带连字符的 `workspace-write`。

```json
{"id":"2","method":"thread/start","params":{"cwd":"/Users/bytedance/ws/agent-cli-demo","approvalPolicy":"never","sandbox":"workspace-write","ephemeral":true}}
```

```json
{"id":"2","result":{"thread":{"id":"thr_123"}}}
```

Codex 的 `sandbox` 用驼峰 `workspaceWrite`，并且官方示例里会指定模型。默认会话会落盘，断线后还能恢复。回包之后通常还会再来一条不带 `id` 的 `thread/started`。

```json
{"id":10,"method":"thread/start","params":{"model":"gpt-5.4","cwd":"/Users/bytedance/ws/agent-cli-demo","approvalPolicy":"never","sandbox":"workspaceWrite"}}
```

```json
{"id":10,"result":{"thread":{"id":"thr_123","sessionId":"thr_123","ephemeral":false,"modelProvider":"openai"}}}
```

```json
{"method":"thread/started","params":{"thread":{"id":"thr_123"}}}
```

`approvalPolicy` 为 `never` 时，执行命令不再反过来问客户端。本仓库的封装用的就是这个值。若改成需要确认，服务端会在同一条连接上发带 `id` 的请求，见文末「服务反过来问客户端」。

### 4. turn/start

把一句用户输入交给已经开好的会话，这一步才调用模型。回包立刻返回，状态是 `inProgress`，`items` 是空的。回答不在这个回包里。

`input` 是内容块数组。`threadId` 用上一步的 `thread.id`。同一条连接上再问下一句，只重发这一条，把 `id` 改掉，`threadId` 保持不变。

trae-cli：

```json
{"id":"3","method":"turn/start","params":{"threadId":"thr_123","cwd":"/Users/bytedance/ws/agent-cli-demo","approvalPolicy":"never","input":[{"type":"text","text":"张三最喜欢吃什么？只回答食物名字。"}]}}
```

```json
{"id":"3","result":{"turn":{"id":"turn_456"}}}
```

Codex 的 `input` 同样是数组，还可以在这一轮临时改模型、工作目录和输出结构。这些覆盖会变成该 thread 后续轮次的默认值。`outputSchema` 只对当前这一轮有效。

```json
{"id":30,"method":"turn/start","params":{"threadId":"thr_123","cwd":"/Users/bytedance/ws/agent-cli-demo","approvalPolicy":"never","input":[{"type":"text","text":"张三最喜欢吃什么？只回答食物名字。"}]}}
```

```json
{"id":30,"result":{"turn":{"id":"turn_456","status":"inProgress","items":[],"error":null}}}
```

### 5. 回答：同一条连接上的通知

从 `turn/start` 的回包之后，继续读这条连接。下面几条都没有 `id`。增量自己按顺序拼；收尾以 `turn/completed` 里的完整文本为准。

`item/agentMessage/delta` 是回答的一小段。`delta` 是新增的字，不是到目前为止的全文。

```json
{"method":"item/agentMessage/delta","params":{"turnId":"turn_456","delta":"红烧"}}
```

```json
{"method":"item/agentMessage/delta","params":{"turnId":"turn_456","delta":"牛肉面"}}
```

`turn/completed` 表示这一轮结束。取 `params.turn.items` 里 `type` 为 `agentMessage` 的 `text`。上面这个问题的答案是「红烧牛肉面」。`phase` 为 `final_answer` 的那一项是最终回答。

```json
{"method":"turn/completed","params":{"turn":{"id":"turn_456","status":"completed","items":[{"type":"agentMessage","text":"红烧牛肉面","phase":"final_answer"}]}}}
```

一轮里还会夹着别的通知，它们不是回答正文：

| 通知 | 干什么 |
|---|---|
| `turn/started` | 这一轮真正开始跑了，`status` 为 `inProgress` |
| `item/started` | 一个工作项开始，例如用户消息、命令、文件修改 |
| `item/completed` | 该工作项的最终状态 |
| `turn/plan/updated` | 代理改了自己的计划 |
| `item/commandExecution/outputDelta` | 命令输出的增量 |
| `turn/diff/updated` | 这一轮文件改动的汇总 diff |

Codex 上这一轮失败时，`turn/completed` 的 `status` 为 `failed`，`turn.error.message` 里是原因。客户端取消后，`status` 为 `interrupted`。

## 同一条连接上的一轮

把上面的报文按时间排开。问句是「张三最喜欢吃什么？只回答食物名字。」

```text
→ initialize
← result（id 对上）
→ initialized
→ thread/start
← result.thread.id = thr_123
→ turn/start（threadId = thr_123）
← result.turn.id = turn_456，items 为空
← item/agentMessage/delta  delta = "红烧"
← item/agentMessage/delta  delta = "牛肉面"
← turn/completed           items[].text = "红烧牛肉面"
```

第二句「他为什么喜欢？」只再发一次 `turn/start`，`threadId` 仍是 `thr_123`。模型能看到上一句。

本仓库的 `main.go` 已经把 trae-cli 的这四步包进 `POST /ask`，默认连 `ws://127.0.0.1:8766`。调用方不用自己拼这些 JSON。

## Codex 多出来的几个方法

trae-cli 这份实测路径里没有下面三条。Codex 用它们处理断线、取消，以及一轮还没跑完时追加一句。

### thread/resume

用上次记下的 `thread.id` 把已经落盘的会话装回内存。回包形状和 `thread/start` 相同，这条连接重新订上该会话的事件。`ephemeral: true` 的会话没有这份历史，恢复不了。

```json
{"id":11,"method":"thread/resume","params":{"threadId":"thr_123"}}
```

```json
{"id":11,"result":{"thread":{"id":"thr_123","ephemeral":false}}}
```

### turn/interrupt

取消正在跑的那一轮。请求本身的回包是空对象，结束看随后的 `turn/completed`，`status` 为 `interrupted`。

```json
{"id":31,"method":"turn/interrupt","params":{"threadId":"thr_123","turnId":"turn_456"}}
```

```json
{"id":31,"result":{}}
```

### turn/steer

往当前这一轮后面追加用户输入，不新开一轮。`expectedTurnId` 必须等于正在跑的 turn id。没有正在跑的轮次时，这个请求会失败。

```json
{"id":32,"method":"turn/steer","params":{"threadId":"thr_123","expectedTurnId":"turn_456","input":[{"type":"text","text":"只回答食物名字。"}]}}
```

```json
{"id":32,"result":{"turnId":"turn_456"}}
```

## 服务反过来问客户端

长连接是双向的。`approvalPolicy` 不是 `never` 时，代理要跑命令或改文件，会发一条带 `id` 的请求过来。客户端用同一个 `id` 回答，这一轮才继续。这是它和 qwen 那条只读 SSE 不一样的地方：SSE 上服务问不了客户端。

命令确认的顺序是：`item/started`，然后 `item/commandExecution/requestApproval`，客户端给出 `accept`、`acceptForSession`、`decline` 或 `cancel`，接着 `serverRequest/resolved`，最后 `item/completed`。文件修改走 `item/fileChange/requestApproval`，决定同样是这四个字符串。

```json
{"id":"srv-1","method":"item/commandExecution/requestApproval","params":{"threadId":"thr_123","turnId":"turn_456","itemId":"item_1","command":"ls","cwd":"/Users/bytedance/ws/agent-cli-demo"}}
```

本仓库的 `main.go` 不回答这类请求。它在 `thread/start` 里把 `approvalPolicy` 设成 `never`，服务端就不会停下来等确认。