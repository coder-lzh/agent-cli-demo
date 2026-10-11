# agent-cli-demo

演示两件事：

1. agent CLI（本项目用 [Qwen Code](https://github.com/QwenLM/qwen-code)）启动时**自动识别项目里的 skill 文件**（`.agents/skills/zhangsan-eating/SKILL.md`），提问命中时自动调用 skill 内容作答；
2. 用 Go 写一个 [main.go](./main.go)，把 `qwen serve` 的 HTTP + SSE 三步流程（建会话 → 提问 → 订阅事件流）**包成一个普通的一问一答接口** `POST /ask`。

## 架构

```
调用方 ── POST /ask ──▶  main.go（:8080）
                          │ ① POST /session                    建会话（只建一次，多轮复用）
                          │ ② POST /session/:id/prompt         发问题，拿 promptId + lastEventId 游标
                          │ ③ GET  /session/:id/events（SSE）  拼 agent_message_chunk 增量，等 turn_complete
                          ▼
                      qwen serve（127.0.0.1:4170） ──▶ Qwen 模型
                          ▲
                      .agents/skills/zhangsan-eating/SKILL.md（提问命中时自动加载）
```

## 目录结构

```
.
├── .agents/
│   └── skills/
│       └── zhangsan-eating/
│           └── SKILL.md      # 演示 skill：《张三吃饭知识库》
├── main.go                   # qwen serve 的 HTTP 封装：POST /ask、GET /health
├── go.mod                    # module agent-cli-demo（无第三方依赖）
├── qwen-code 接入说明.md      # 完整接入文档
└── README.md
```

## 快速开始

前置：Node.js ≥ 22、Go ≥ 1.25。

**1. 安装并登录 Qwen Code**（个人免费：Qwen OAuth，或百炼 API Key）

```bash
npm install -g @qwen-code/qwen-code@latest
qwen          # 进入交互模式后执行 /auth 登录
```

**2. 启动 qwen serve**（`--workspace` 必须是项目目录，skill 才挂得上）

```bash
qwen serve --no-web --port 4170 --workspace E:/go_ws/agent-cli-demo
```

**3. 启动封装服务**

```bash
go run . -addr :8080
```

**4. 提问**

```bash
curl -X POST http://127.0.0.1:8080/ask \
  -H 'Content-Type: application/json' \
  -d '{"question":"张三最喜欢吃什么？只回答食物名字。"}'
```

```json
{"answer":"红烧牛肉面","duration_ms":4545,"source":"qwen-serve"}
```

## API

| 接口 | 说明 |
|---|---|
| `POST /ask` | 请求 `{"question":"..."}`；返回 `{"answer","error","duration_ms","source"}` |
| `GET /health` | 连接状态、会话 sessionId、会话工作区 |

`main.go` 参数：`-addr`（默认 `:8080`）、`-qwen`（qwen serve 地址，默认 `http://127.0.0.1:4170`）、`-timeout`（单问题超时秒数，默认 120）。

## 实测效果（qwen 0.25.1，2026-10-11）

| 提问 | 回答 | 说明 |
|---|---|---|
| 张三最喜欢吃什么？只回答食物名字。 | 红烧牛肉面 | 答案出自 SKILL.md，skill 被自动识别 |
| 他为什么喜欢？用一句话回答。 | 因为小时候妈妈经常做，有情感回忆，加上口感层次丰富、饱腹感强又实惠。 | 会话复用，追问能结合上文 |

## 原理与文档

- `qwen serve` 是 Qwen Code 自带的本地 HTTP 服务，回答通过 SSE（`text/event-stream`）以增量片段推送，需要客户端自己拼接；完整的三步 curl、SSE 事件格式、安装登录细节见 **[qwen-code 接入说明.md](qwen-code%20接入说明.md)**。
- skill 目录约定：Qwen Code 同时扫描 `.qwen/skills/` 和 `.agents/skills/`，无需任何启用参数（源码常量 `PROJECT_SKILL_DIRS=[".qwen",".agents"]`）。
- 背景：最初方案是 trae-cli app-server（WebSocket JSON-RPC），因需企业版账号改用 Qwen Code。

> 注：`qwen serve` 官方标记为 Stage 1 实验特性，接口可能随版本变化。
