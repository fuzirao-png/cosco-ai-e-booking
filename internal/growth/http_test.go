package growth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func makeStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(context.Background(), filepath.Join(t.TempDir(), "growth.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func testApp(t *testing.T, model *Model, embedder Embedder) (*Server, *Store) {
	t.Helper()
	store := makeStore(t)
	index, err := NewIndex(context.Background(), embedder)
	if err != nil {
		t.Fatal(err)
	}
	mode := "local"
	if embedder != nil {
		mode = "bge"
	}
	return NewServer(Agent{store, index}, model, mode, ""), store
}

func request(t *testing.T, app http.Handler, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, httptest.NewRequest(method, path, bytes.NewReader(data)))
	if recorder.Code != status {
		t.Fatalf("%s %s status = %d, want %d: %s", method, path, recorder.Code, status, recorder.Body.String())
	}
	return recorder
}

func open(t *testing.T, app http.Handler, leadID string) (string, string) {
	t.Helper()
	openResponse := request(t, app, "/api/calls", map[string]string{"lead_id": leadID}, 201)
	var body struct{ ID, Greeting string }
	if err := json.Unmarshal(openResponse.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("open call: %s (%v)", openResponse.Body, err)
	}
	return "/api/calls/" + body.ID, body.Greeting
}

type event struct {
	Epoch   int            `json:"epoch"`
	Intent  string         `json:"intent"`
	Status  string         `json:"status"`
	Text    string         `json:"text"`
	Result  map[string]any `json:"result"`
	Sources []Citation     `json:"sources"`
}

func decodeEvent(t *testing.T, frame string) (string, event) {
	t.Helper()
	kind, raw, found := strings.Cut(frame, "\ndata: ")
	var event event
	if !found || !strings.HasPrefix(kind, "event: ") {
		t.Fatalf("malformed SSE frame %q", frame)
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("SSE: %v", err)
	}
	return strings.TrimPrefix(kind, "event: "), event
}

func utterance(t *testing.T, app http.Handler, id, text string) (event, map[string][]event) {
	t.Helper()
	response := request(t, app, id+"/turns", map[string]string{"asr_text": text}, 200)
	if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("SSE Content-Type missing")
	}
	events := map[string][]event{}
	for _, raw := range strings.Split(strings.TrimSpace(response.Body.String()), "\n\n") {
		kind, item := decodeEvent(t, raw)
		events[kind] = append(events[kind], item)
	}
	if len(events["tool"]) != 1 || len(events["done"]) != 1 {
		t.Fatalf("unexpected SSE events: %v", events)
	}
	return events["tool"][0], events
}

func TestBookingFlowAndIdempotency(t *testing.T) {
	app, store := testApp(t, nil, nil)
	id, _ := open(t, app, "harbor")
	cases := []struct {
		text, intent, status, key string
		value                     any
	}{
		{"确认认证", "verify", "blocked", "", nil},
		{"确认注册 海岚国际货运", "register", "ok", "stage", "registered"},
		{"确认认证", "verify", "ok", "stage", "verified"},
		{"查价 上海到新加坡", "quote", "needs_input", "", nil},
		{"40HC ２箱", "quote", "ok", "total", float64(1380)},
		{"确认订舱 WRONG", "confirm", "needs_input", "", nil},
	}
	for _, item := range cases {
		tool, sse := utterance(t, app, id, item.text)
		if tool.Intent != item.intent || tool.Status != item.status || (item.key != "" && tool.Result[item.key] != item.value) {
			t.Fatalf("%q: %+v", item.text, tool)
		}
		if item.key == "total" && (len(sse["delta"]) < 2 || len(tool.Sources) != 1 || tool.Sources[0].ID != "rate-CNSHA-SGSIN-40HC") {
			t.Fatalf("quote missing chunks or citation: %+v", sse)
		}
	}
	preview, _ := utterance(t, app, id, "申请订舱")
	confirmation, ok := preview.Result["confirmation"].(string)
	if !ok || preview.Status != "preview" {
		t.Fatalf("invalid preview: %+v", preview)
	}
	first, _ := utterance(t, app, id, confirmation)
	second, _ := utterance(t, app, id, confirmation)
	ref, ok := first.Result["reference"].(string)
	if !ok || !strings.HasPrefix(ref, "BK-") || second.Result["reference"] != ref {
		t.Fatalf("duplicate confirmation did not return same reference: %+v / %+v", first, second)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM bookings").Scan(&count); err != nil || count != 1 {
		t.Fatalf("booking count = %d (%v)", count, err)
	}
}

func TestCrossCallMemory(t *testing.T) {
	app, store := testApp(t, nil, nil)
	id, _ := open(t, app, "sail")
	original := "我卡在认证材料没准备好。"
	utterance(t, app, id, original)
	response := request(t, app, id+"/end", struct{}{}, 200)
	if !strings.Contains(response.Body.String(), `"profile_update_queued":true`) {
		t.Fatal(response.Body.String())
	}
	next, greeting := open(t, app, "sail")
	if !strings.Contains(greeting, "上次您提到「认证材料没准备好」") {
		t.Fatal(greeting)
	}
	tool, _ := utterance(t, app, next, "继续处理上次的问题")
	if tool.Intent != "memory" {
		t.Fatalf("expected memory reply, got %+v", tool)
	}
	_, profile, err := readProfile(context.Background(), store.db, "sail")
	if err != nil || profile.Evidence["current_blocker"].Quote != original {
		t.Fatalf("profile = %+v (%v)", profile, err)
	}
}

func TestOptOut(t *testing.T) {
	app, _ := testApp(t, nil, nil)
	id, _ := open(t, app, "sail")
	tool, _ := utterance(t, app, id, "请不要再打了")
	if tool.Intent != "opt_out" {
		t.Fatalf("expected opt-out: %+v", tool)
	}
	request(t, app, "/api/calls", map[string]string{"lead_id": "sail"}, 409)
	request(t, app, id+"/turns", map[string]string{"asr_text": "认证状态"}, 409)
	request(t, app, id+"/end", struct{}{}, 200)
}

func TestInterruptionCancelsUpstreamAndStopsOldDeltas(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Stream bool }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		if !payload.Stream {
			w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"select_read_skill","arguments":"{\"intent\":\"knowledge\"}"}}]}}]}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"第一段\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-unblock:
		}
	}))
	defer upstream.Close()
	defer close(unblock)
	app, _ := testApp(t, newModel(upstream.URL, "", "test"), nil)
	id, _ := open(t, app, "harbor")
	server := httptest.NewServer(app)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+id+"/turns", strings.NewReader(`{"asr_text":"运价包含哪些费用？"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	var lines []string
	found := false
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lines = append(lines, line)
			continue
		}
		kind, item := decodeEvent(t, strings.Join(lines, "\n"))
		lines = nil
		if kind == "delta" {
			if item.Text != "第一段" || item.Epoch != 1 {
				t.Fatalf("unexpected delta: %+v", item)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("missing delta: %v", scanner.Err())
	}
	interrupt := request(t, app, id+"/interrupt", map[string]string{"asr_partial_token": "等一下"}, 200)
	if !strings.Contains(interrupt.Body.String(), `"epoch":2`) {
		t.Fatal(interrupt.Body.String())
	}
	select {
	case <-upstreamCanceled:
	case <-ctx.Done():
		t.Fatal("interrupt did not cancel upstream")
	}
	if scanner.Scan() || scanner.Err() != nil {
		t.Fatalf("old SSE did not end cleanly: %q (%v)", scanner.Text(), scanner.Err())
	}
}

func TestModelFunctionCallingAndEmbeddingHTTP(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer demo" {
			t.Error("missing configured credential")
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		switch r.URL.Path {
		case "/chat/completions":
			if len(payload["tools"]) == 0 {
				t.Error("function schema missing")
			}
			io.WriteString(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"select_read_skill","arguments":"{\"intent\":\"confirm\"}"}}]}}]}`)
		case "/embeddings":
			if len(payload["input"]) == 0 {
				t.Error("embedding input missing")
			}
			io.WriteString(w, `{"data":[{"index":1,"embedding":[0,2]},{"index":0,"embedding":[3,0]}]}`)
		default:
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
		}
	}))
	defer api.Close()
	model := newModel(api.URL, "demo", "test")
	intent, err := model.chooseSkill(context.Background(), "确认订舱")
	if err != nil || intent != "knowledge" {
		t.Fatalf("unexpected model intent %q (%v)", intent, err)
	}
	embeddings, err := model.Embed(context.Background(), []string{"上海", "新加坡"})
	if err != nil || len(embeddings) != 2 || embeddings[0][0] != 1 || embeddings[1][1] != 1 {
		t.Fatalf("unexpected embeddings %v (%v)", embeddings, err)
	}
}
