package asr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// v2 断点续传：query 携带 enable_speaker_context / speaker_context_id。
func TestSignatureParamsSpeakerContextQuery(t *testing.T) {
	p := common.NewSignatureParams(1400000000, "bigmodel", "v1")
	p.SpeakerDiarization = SpeakerDiarizationCluster
	p.EnableSpeakerContext = SpeakerContextSync
	p.SpeakerContextID = "abc123"

	query := p.BuildQueryString()
	if !strings.Contains(query, "enable_speaker_context=1") {
		t.Fatalf("query missing enable_speaker_context=1: %s", query)
	}
	if !strings.Contains(query, "speaker_context_id=abc123") {
		t.Fatalf("query missing speaker_context_id: %s", query)
	}

	// 关闭时两个参数都不下发；只带 id 也不下发 id。
	p2 := common.NewSignatureParams(1400000000, "bigmodel", "v1")
	p2.SpeakerDiarization = SpeakerDiarizationCluster
	p2.SpeakerContextID = "abc123"
	if q2 := p2.BuildQueryString(); strings.Contains(q2, "speaker_context") {
		t.Fatalf("context off must not emit context params: %s", q2)
	}
}

// v2 断点续传：本地校验（模式取值、必须与说话人分离同开）。
func TestSpeakerContextValidation(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	r.SetEnableSpeakerContext(3) // 非法模式
	if err := r.Start(); err == nil || !strings.Contains(err.Error(), "EnableSpeakerContext") {
		t.Fatalf("expected EnableSpeakerContext validation error, got %v", err)
	}

	r2 := newRecognizerForTest(listener)
	r2.SetEnableSpeakerContext(SpeakerContextSync) // 不开说话人分离
	if err := r2.Start(); err == nil || !strings.Contains(err.Error(), "SetSpeakerDiarization") {
		t.Fatalf("expected diarization requirement error, got %v", err)
	}
}

// v2 断点续传：首响应的 speaker_continue 解析并可通过 SpeakerContinue() 读取。
func TestReadLoopCapturesSpeakerContinue(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	first := `{"code":0,"message":"success","voice_id":"v1",` +
		`"speaker_continue":{"continue_status":"resumed","speaker_context_id":"abc123"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(first)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}
	if err := server.WriteMessage(websocket.TextMessage, []byte(`{"code":0,"message":"success","voice_id":"v1","final":1,"result":{"slice_type":2,"index":0}}`)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sc := r.SpeakerContinue(); sc != nil {
			if sc.ContinueStatus != "resumed" || sc.SpeakerContextID != "abc123" {
				t.Fatalf("unexpected speaker_continue: %+v", sc)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SpeakerContinue was not captured")
}

// 复用现有 mock：确保签名参数确实进入 v2 握手 query（走真实 Start 路径）。
func TestStartQueryCarriesSpeakerContext(t *testing.T) {
	var gotQuery atomic.Value
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery.Store(r.URL.RawQuery)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte(`{"code":0,"message":"success","voice_id":"v1","final":1,"result":{"slice_type":2,"index":0}}`))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	listener := newTestListener()
	r := newRecognizerForTest(listener)
	r.SetEndpoint("ws" + strings.TrimPrefix(server.URL, "http"))
	r.SetSpeakerDiarization(SpeakerDiarizationCluster)
	r.SetEnableSpeakerContext(SpeakerContextSync)
	r.SetSpeakerContextID("feedface")
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	r.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q, ok := gotQuery.Load().(string); ok && q != "" {
			if !strings.Contains(q, "enable_speaker_context=1") ||
				!strings.Contains(q, "speaker_context_id=feedface") {
				t.Fatalf("query missing context params: %s", q)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never observed the handshake query")

	// 防止编译器报未使用（json 仅在扩展解析时使用）
	_ = json.Marshal
}
