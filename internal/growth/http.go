package growth

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

//go:embed openapi.json
var openAPI []byte

const apiDocs = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>E-Booking Growth Agent API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css"></head>
<body><div id="swagger-ui"><a href="/openapi.json">OpenAPI JSON</a></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url:'/openapi.json',dom_id:'#swagger-ui',validatorUrl:null})</script></body></html>`

type Server struct {
	agent     Agent
	model     *Model
	embedding string
	mux       *http.ServeMux
	mu        sync.Mutex
	sessions  map[string]*Session
	operation sync.Mutex
	workReady chan struct{}
}

func NewServer(agent Agent, model *Model, embedding, staticDir string) *Server {
	s := &Server{
		agent: agent, model: model, embedding: embedding, mux: http.NewServeMux(),
		sessions: make(map[string]*Session), workReady: make(chan struct{}, 1),
	}
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/leads", s.listLeads)
	s.mux.HandleFunc("POST /api/calls", s.openCall)
	s.mux.HandleFunc("POST /api/calls/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("POST /api/calls/{id}/end", s.endCall)
	s.mux.HandleFunc("POST /api/calls/{id}/turns", s.turn)
	s.mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(openAPI)
	})
	s.mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, apiDocs)
	})
	if staticDir != "" {
		files := http.FileServer(http.Dir(staticDir))
		s.mux.Handle("GET /", files)
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) RunWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := s.agent.Store.applyPending(ctx, ""); err != nil && ctx.Err() == nil {
			slog.Error("apply queued profile updates", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.workReady:
		}
	}
}

func sendJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func problem(w http.ResponseWriter, status int, detail string) {
	sendJSON(w, status, map[string]string{"detail": detail})
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() == nil {
		slog.Error("request failed", "path", r.URL.Path, "error", err)
	}
	problem(w, 500, "服务暂不可用")
}

func readBody(w http.ResponseWriter, r *http.Request, result any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	if err := decoder.Decode(result); err != nil || decoder.Decode(new(any)) != io.EOF {
		problem(w, 422, "请输入有效的 JSON")
		return false
	}
	return true
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	model := "local"
	if s.model != nil {
		model = "configured"
	}
	sendJSON(w, 200, map[string]string{"status": "ok", "model": model, "embedding": s.embedding})
}

func (s *Server) listLeads(w http.ResponseWriter, r *http.Request) {
	type leadView struct {
		Lead
		Profile  Profile `json:"profile"`
		OptedOut bool    `json:"opted_out"`
	}
	results := make([]leadView, 0, len(leads))
	for _, lead := range leads {
		stage, profile, err := readProfile(r.Context(), s.agent.Store.db, lead.ID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		optedOut, err := s.agent.Store.optedOut(r.Context(), lead.ID)
		if err != nil {
			internalError(w, r, err)
			return
		}
		lead.Stage = stage
		results = append(results, leadView{lead, profile, optedOut})
	}
	sendJSON(w, 200, results)
}

func (s *Server) openCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LeadID string `json:"lead_id"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if body.LeadID == "" {
		problem(w, 422, "请输入客户线索 ID")
		return
	}
	lead, exists := findLead(body.LeadID)
	if !exists {
		problem(w, 404, "未找到线索")
		return
	}
	ctx := r.Context()
	if err := s.agent.Store.applyPending(ctx, lead.ID); err != nil {
		internalError(w, r, err)
		return
	}

	s.operation.Lock()
	defer s.operation.Unlock()
	optedOut, err := s.agent.Store.optedOut(ctx, lead.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	if optedOut {
		problem(w, 409, "该线索已选择停止联系")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= 64 {
		problem(w, 429, "演示通话已满，请结束已有通话")
		return
	}
	_, profile, err := readProfile(ctx, s.agent.Store.db, lead.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	id, err := newID(16)
	if err != nil {
		internalError(w, r, err)
		return
	}
	seq, err := s.agent.Store.openCall(ctx, lead.ID)
	if err != nil {
		internalError(w, r, err)
		return
	}
	s.sessions[id] = NewSession(lead.ID, seq, profile)
	greeting := lead.Contact + "您好，我是 E-Booking 演示助手，可以协助您查价、认证和订舱。"
	if profile.CurrentBlocker != nil && *profile.CurrentBlocker != "" {
		greeting += fmt.Sprintf("上次您提到「%s」，我们可以接着处理。", *profile.CurrentBlocker)
	}
	sendJSON(w, 201, map[string]any{"id": id, "greeting": greeting, "profile": profile, "epoch": 0})
}

// lockSession returns a locked active session; its caller must unlock it.
func (s *Server) lockSession(w http.ResponseWriter, r *http.Request) *Session {
	s.mu.Lock()
	session := s.sessions[r.PathValue("id")]
	s.mu.Unlock()
	if session != nil {
		session.mu.Lock()
		if !session.closed {
			return session
		}
		session.mu.Unlock()
	}
	problem(w, 404, "该通话已结束或不存在")
	return nil
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Partial string `json:"asr_partial_token"`
	}
	if !readBody(w, r, &body) {
		return
	}
	if utf8.RuneCountInString(body.Partial) > 1200 {
		problem(w, 422, "中间文本超出长度上限")
		return
	}
	session := s.lockSession(w, r)
	if session == nil {
		return
	}
	defer session.mu.Unlock()
	session.bump()
	session.partial = body.Partial
	sendJSON(w, 200, map[string]any{"epoch": session.epoch, "vad_state": "speech_started"})
}

func (s *Server) endCall(w http.ResponseWriter, r *http.Request) {
	session := s.lockSession(w, r)
	if session == nil {
		return
	}
	defer session.mu.Unlock()
	job := profileJob{Updates: make([]profileUpdate, 0, len(session.updates)), Transcript: session.transcript}
	for _, update := range session.updates {
		job.Updates = append(job.Updates, update)
	}
	queued, err := s.agent.Store.enqueue(r.Context(), session.seq, job)
	if err != nil {
		internalError(w, r, err)
		return
	}
	session.closed = true
	session.bump()
	s.mu.Lock()
	delete(s.sessions, r.PathValue("id"))
	s.mu.Unlock()
	if queued {
		select {
		case s.workReady <- struct{}{}:
		default:
		}
	}
	sendJSON(w, 200, map[string]any{"status": "ended", "profile_update_queued": queued})
}

var sentencePattern = regexp.MustCompile(`[^。！？]+[。！？]?|[。！？]`)
var errStale = errors.New("turn interrupted")

func (s *Server) turn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"asr_text"`
	}
	if !readBody(w, r, &body) {
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" || utf8.RuneCountInString(body.Text) > 1200 {
		problem(w, 422, "请输入 1 至 1200 个字符")
		return
	}
	session := s.lockSession(w, r)
	if session == nil {
		return
	}
	optedOut, err := s.agent.Store.optedOut(r.Context(), session.leadID)
	if err != nil {
		session.mu.Unlock()
		internalError(w, r, err)
		return
	}
	if optedOut {
		session.mu.Unlock()
		problem(w, 409, "该线索已选择停止联系，请结束通话")
		return
	}
	if session.utterance >= 100 {
		session.mu.Unlock()
		problem(w, 429, "已达到此通话的轮次上限，请结束通话")
		return
	}
	session.begin(text)
	epoch, utterance := session.epoch, session.utterance
	intent := explicitIntent(text, session)
	ctx, cancel := context.WithCancel(r.Context())
	session.cancel = cancel
	session.mu.Unlock()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	emit := func(kind string, data map[string]any) error {
		session.mu.Lock()
		defer session.mu.Unlock()
		if session.epoch != epoch || ctx.Err() != nil {
			return errStale
		}
		if data == nil {
			data = map[string]any{}
		}
		data["epoch"] = epoch
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}
		controller.SetWriteDeadline(time.Now().Add(4 * time.Second))
		if _, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, payload); err != nil {
			return err
		}
		return controller.Flush()
	}
	if emit("turn", map[string]any{"utterance_seq": utterance}) != nil {
		return
	}

	if intent == "" && s.model != nil && !sensitivePattern.MatchString(text) {
		intent, err = s.model.chooseSkill(ctx, text)
		if err != nil && emit("notice", map[string]any{"message": "模型路由暂不可用，已切换到本地流程。"}) != nil {
			return
		}
	}
	session.mu.Lock()
	if session.epoch != epoch || ctx.Err() != nil {
		session.mu.Unlock()
		return
	}
	s.operation.Lock()
	result, err := s.agent.decide(ctx, session, text, intent)
	s.operation.Unlock()
	session.mu.Unlock()
	if err != nil {
		message := "服务暂不可用，请重试。"
		if errors.Is(err, errOptedOut) {
			message = "该线索已选择停止联系，请结束通话。"
		} else if ctx.Err() == nil {
			slog.Error("execute skill", "error", err)
		}
		if emit("notice", map[string]any{"message": message}) == nil {
			emit("done", nil)
		}
		return
	}
	result, err = s.agent.enrich(ctx, result, text)
	if err != nil && emit("notice", map[string]any{"message": "向量服务暂不可用，已切换到本地检索。"}) != nil {
		return
	}
	citations := make([]Citation, 0, len(result.Sources))
	for _, source := range result.Sources {
		citations = append(citations, source.Citation)
	}
	if emit("tool", map[string]any{"intent": result.Intent, "status": result.Status, "result": result.Data, "sources": citations}) != nil {
		return
	}
	if s.model != nil && result.Intent == "knowledge" && len(result.Sources) > 0 {
		var evidence strings.Builder
		for _, source := range result.Sources {
			fmt.Fprintf(&evidence, "[%s] %s\n", source.ID, source.Text)
		}
		emitted := false
		err := s.model.streamKnowledge(ctx, text, shorten(evidence.String(), 3600), func(token string) error {
			emitted = true
			return emit("delta", map[string]any{"text": token})
		})
		if err != nil {
			if emit("notice", map[string]any{"message": "模型连接已中断，以下为演示知识库原文。"}) != nil {
				return
			}
			if emitted && emit("delta", map[string]any{"text": "\n\n" + result.Text}) != nil {
				return
			}
		}
		if emitted {
			emit("done", nil)
			return
		}
	}
	streamText(ctx, result.Text, emit)
}

func streamText(ctx context.Context, text string, emit func(string, map[string]any) error) {
	ticker := time.NewTicker(32 * time.Millisecond)
	defer ticker.Stop()
	for _, sentence := range sentencePattern.FindAllString(text, -1) {
		runes := []rune(sentence)
		for start := 0; start < len(runes); start += 10 {
			if emit("delta", map[string]any{"text": string(runes[start:min(len(runes), start+10)])}) != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}
	emit("done", nil)
}
