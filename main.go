package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	listenAddr = flag.String("addr", ":8080", "HTTP 监听地址")
	qwenURL    = flag.String("qwen", "http://127.0.0.1:4170", "qwen serve 服务地址")
	timeout    = flag.Int("timeout", 120, "单请求超时秒数")
)

type AskRequest struct {
	Question string `json:"question"`
}

type AskResponse struct {
	Answer   string `json:"answer"`
	Error    string `json:"error,omitempty"`
	Duration int64  `json:"duration_ms"`
	Source   string `json:"source,omitempty"`
}

// session 是对 qwen serve 一个会话的封装：会话只建一次，每个问题复用同一个 sessionId，
// 模型才能看到之前问过什么。每次提问 = POST /prompt 拿游标 + GET /events 读到 turn_complete。
type session struct {
	askMu   sync.Mutex // 同一时间只允许一个提问；事件流按 promptId 区分属于谁
	stateMu sync.Mutex
	client  *http.Client
	base    string

	sessionID string
	workspace string
}

func newSession(base string) *session {
	return &session{
		client: &http.Client{},
		base:   strings.TrimRight(base, "/"),
	}
}

func main() {
	flag.Parse()

	sess := newSession(*qwenURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	err := sess.ensureSession(ctx)
	cancel()
	if err != nil {
		log.Printf("qwen serve 尚未连上 (%v)。先执行: qwen serve --workspace <项目目录>", err)
	} else {
		id, workspace := sess.state()
		log.Printf("qwen serve 已连接，会话 %s，工作目录 %s", id, workspace)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ask", func(w http.ResponseWriter, r *http.Request) {
		askHandler(sess, w, r)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		healthHandler(sess, w, r)
	})

	server := &http.Server{
		Addr:              *listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("agent-api starting on %s", *listenAddr)
	log.Printf("  qwen:    %s", *qwenURL)
	log.Printf("  timeout: %ds", *timeout)
	log.Printf("  API:     POST /ask, GET /health")

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func askHandler(sess *session, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req AskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, AskResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	defer r.Body.Close()

	question := strings.TrimSpace(req.Question)
	if question == "" {
		writeJSON(w, http.StatusBadRequest, AskResponse{Error: "question is required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(*timeout)*time.Second)
	defer cancel()

	answer, err := sess.ask(ctx, question)
	resp := AskResponse{
		Answer:   answer,
		Duration: time.Since(start).Milliseconds(),
		Source:   "qwen-serve",
	}
	if err != nil {
		resp.Error = err.Error()
	}
	log.Printf("问题: %s (%dms)", question, resp.Duration)
	writeJSON(w, http.StatusOK, resp)
}

func healthHandler(sess *session, w http.ResponseWriter, r *http.Request) {
	sessionID, workspace := sess.state()
	status := "ok"
	if sessionID == "" {
		status = "disconnected"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    status,
		"qwen":      *qwenURL,
		"connected": sessionID != "",
		"sessionId": sessionID,
		"workspace": workspace,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ask 走完一问一答：确保有会话 -> 发问题 -> 订阅事件流读到 turn_complete。
func (s *session) ask(ctx context.Context, question string) (string, error) {
	s.askMu.Lock()
	defer s.askMu.Unlock()

	if err := s.ensureSession(ctx); err != nil {
		return "", err
	}
	sessionID, _ := s.state()

	prompt, err := s.sendPrompt(ctx, sessionID, question)
	if err != nil {
		var he *httpError
		if errors.As(err, &he) && he.status == http.StatusNotFound {
			// 会话被服务端回收（闲置超时），重建会话后重试一次
			s.setState("", "")
			if err := s.ensureSession(ctx); err != nil {
				return "", err
			}
			sessionID, _ = s.state()
			if prompt, err = s.sendPrompt(ctx, sessionID, question); err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}
	return s.waitAnswer(ctx, sessionID, prompt)
}

type sessionInfo struct {
	SessionID    string `json:"sessionId"`
	WorkspaceCwd string `json:"workspaceCwd"`
}

func (s *session) ensureSession(ctx context.Context) error {
	if id, _ := s.state(); id != "" {
		return nil
	}
	var info sessionInfo
	if err := s.postJSON(ctx, "/session", map[string]string{"sessionScope": "thread"}, &info); err != nil {
		return err
	}
	if info.SessionID == "" {
		return errors.New("qwen serve 未返回 sessionId")
	}
	s.setState(info.SessionID, info.WorkspaceCwd)
	return nil
}

type promptInfo struct {
	PromptID    string `json:"promptId"`
	LastEventID int64  `json:"lastEventId"`
}

func (s *session) sendPrompt(ctx context.Context, sessionID, question string) (promptInfo, error) {
	var out promptInfo
	body := map[string]any{
		"prompt": []map[string]string{{"type": "text", "text": question}},
	}
	err := s.postJSON(ctx, "/session/"+sessionID+"/prompt", body, &out)
	return out, err
}

// waitAnswer 从 prompt 返回的游标处订阅 SSE，直到收到本次提问自己的 turn_complete。
// prompt 先返回、事件后产生，中间空档由服务端按 Last-Event-ID 补发，不会丢事件。
func (s *session) waitAnswer(ctx context.Context, sessionID string, p promptInfo) (string, error) {
	url := s.base + "/session/" + sessionID + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", strconv.FormatInt(p.LastEventID, 10))

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return "", &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}

	var answer strings.Builder
	reader := bufio.NewReaderSize(resp.Body, 1<<16)
	event := ""
	var dataLines []string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return answer.String(), fmt.Errorf("等待回答超时: %w", cerr)
			}
			return answer.String(), fmt.Errorf("事件流中断: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")

		switch {
		case line == "":
			done, herr := handleEvent(event, strings.Join(dataLines, "\n"), p.PromptID, &answer)
			event, dataLines = "", nil
			if herr != nil {
				return answer.String(), herr
			}
			if done {
				return answer.String(), nil
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// 其余行（": heartbeat"、retry:、id: 等）忽略
	}
}

// envelope 是 SSE 事件 data 字段的结构，只声明用得到的部分。
type envelope struct {
	PromptID string `json:"promptId"`
	Data     struct {
		StopReason string `json:"stopReason"`
		Update     struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Text string `json:"text"`
			} `json:"content"`
			Meta struct {
				ExecutionLifecycle struct {
					SubagentID string `json:"subagentId"`
				} `json:"executionLifecycle"`
			} `json:"_meta"`
		} `json:"update"`
	} `json:"data"`
}

// handleEvent 处理一条完整 SSE 事件：agent_message_chunk 的 text 是增量片段，拼起来才是完整回答；
// 收到本次提问自己的 turn_complete 就结束。done=true 表示读完了。
func handleEvent(event, data, promptID string, answer *strings.Builder) (bool, error) {
	switch event {
	case "session_update", "turn_complete":
	default:
		return false, nil
	}
	var env envelope
	if err := json.Unmarshal([]byte(data), &env); err != nil {
		return false, nil
	}
	if env.PromptID != promptID {
		return false, nil
	}
	if event == "turn_complete" {
		if stop := env.Data.StopReason; stop != "" && stop != "end_turn" && answer.Len() == 0 {
			return true, fmt.Errorf("turn 结束但未产生回答 (stopReason=%s)", stop)
		}
		return true, nil
	}
	u := env.Data.Update
	if u.SessionUpdate != "agent_message_chunk" || u.Content.Text == "" {
		return false, nil
	}
	if u.Meta.ExecutionLifecycle.SubagentID != "" {
		return false, nil // 子代理（如自动记忆）的输出不算答案
	}
	answer.WriteString(u.Content.Text)
	return false, nil
}

type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func (s *session) postJSON(ctx context.Context, path string, in, out any) error {
	buf, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &httpError{status: resp.StatusCode, body: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (s *session) state() (sessionID, workspace string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionID, s.workspace
}

func (s *session) setState(sessionID, workspace string) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.sessionID = sessionID
	s.workspace = workspace
}
