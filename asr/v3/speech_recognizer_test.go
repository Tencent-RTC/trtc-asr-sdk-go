package v3

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
	"github.com/gorilla/websocket"
)

type failEvent struct {
	resp *SpeechRecognitionResponse
	err  error
}

type testListener struct {
	mu         sync.Mutex
	startN     int
	sentenceN  int
	changeN    int
	endN       int
	completeN  int
	failN      int
	sentenceCh chan *SpeechRecognitionResponse
	failCh     chan failEvent
	completeCh chan *SpeechRecognitionResponse
}

func newTestListener() *testListener {
	return &testListener{
		sentenceCh: make(chan *SpeechRecognitionResponse, 8),
		failCh:     make(chan failEvent, 8),
		completeCh: make(chan *SpeechRecognitionResponse, 8),
	}
}

func (l *testListener) OnRecognitionStart(resp *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.startN++
	l.mu.Unlock()
}

func (l *testListener) OnSentenceBegin(resp *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.sentenceN++
	l.mu.Unlock()
	select {
	case l.sentenceCh <- resp:
	default:
	}
}

func (l *testListener) OnRecognitionResultChange(_ *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.changeN++
	l.mu.Unlock()
}

func (l *testListener) OnSentenceEnd(_ *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.endN++
	l.mu.Unlock()
}

func (l *testListener) OnRecognitionComplete(resp *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.completeN++
	l.mu.Unlock()
	select {
	case l.completeCh <- resp:
	default:
	}
}

func (l *testListener) OnFail(resp *SpeechRecognitionResponse, err error) {
	l.mu.Lock()
	l.failN++
	l.mu.Unlock()
	select {
	case l.failCh <- failEvent{resp: resp, err: err}:
	default:
	}
}

func newRecognizerForTest(t *testing.T, listener SpeechRecognitionListener) *SpeechRecognizer {
	t.Helper()
	cred := NewCredential(1400000000, "test-secret")
	r := NewSpeechRecognizer(cred, "16k_zh_en", listener)
	r.SetWriteTimeout(500 * time.Millisecond)
	r.SetStopTimeout(2 * time.Second)
	return r
}

// v3Server is a mock /asr/v3 endpoint. It captures the handshake URL and the
// start frame, then runs the scripted behavior.
type v3Server struct {
	srv *httptest.Server

	mu            sync.Mutex
	handshakeURL  *url.URL
	startFrameRaw []byte
	audioFrames   [][]byte
	gotEnd        chan struct{}
}

// startV3Server starts a mock server. onStartFrame is invoked after the start
// frame is read: return the immediate response frame (typically the ack) or
// nil to close without answering.
func startV3Server(t *testing.T, onStartFrame func(conn *websocket.Conn, s *v3Server)) *v3Server {
	t.Helper()
	s := &v3Server{gotEnd: make(chan struct{}, 1)}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.handshakeURL = r.URL
		s.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		mt, frame, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read start frame failed: %v", err)
			return
		}
		if mt != websocket.TextMessage {
			t.Errorf("start frame must be text, got %d", mt)
			return
		}
		s.mu.Lock()
		s.startFrameRaw = frame
		s.mu.Unlock()

		if onStartFrame != nil {
			onStartFrame(conn, s)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *v3Server) wsURL() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

func (s *v3Server) getStartFrame() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startFrameRaw
}

func (s *v3Server) getHandshakeURL() *url.URL {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handshakeURL
}

// readAudioUntilEnd drains frames until the {"type":"end"} text frame,
// recording binary frames as audio.
func (s *v3Server) readAudioUntilEnd(conn *websocket.Conn) {
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			s.mu.Lock()
			s.audioFrames = append(s.audioFrames, data)
			s.mu.Unlock()
			continue
		}
		var ctrl struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &ctrl) == nil && ctrl.Type == "end" {
			s.gotEnd <- struct{}{}
			return
		}
	}
}

func writeJSON(t *testing.T, conn *websocket.Conn, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame failed: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write frame failed: %v", err)
	}
}

func ackFrame(voiceID string) map[string]interface{} {
	return map[string]interface{}{"code": 0, "message": "success", "voice_id": voiceID}
}

func resultFrame(sliceType, final int, text string) map[string]interface{} {
	return map[string]interface{}{
		"code": 0, "message": "success", "voice_id": "v1", "final": final,
		"result": map[string]interface{}{
			"slice_type": sliceType, "index": 0, "start_time": 0, "end_time": 1000,
			"voice_text_str": text,
		},
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// TestStartFrameWire verifies the start frame is the only place carrying auth
// and params: the handshake URL has just voice_id, and the frame is
// {type:start, auth, params} in snake_case.
func TestStartFrameWire(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("voice-1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	cred := NewCredential(1400000000, "test-secret")
	r := NewSpeechRecognizer(cred, "16k_zh_en", newTestListener())
	r.SetEndpoint(s.wsURL())
	r.SetVoiceID("voice-1")
	r.SetHotwordList("深度学习|10")
	r.SetVadLevel(0)
	r.SetNoiseThreshold(1.5)
	r.SetFilterEmptyResult(0)
	r.SetConvertNumMode(0) // explicit 0 must be sent on v3
	r.SetSpeakerDiarization(SpeakerDiarizationVoiceprint)
	r.SetSpeakerRoles([]SpeakerRole{{RoleName: "teacher", AudioURL: "https://example.com/t.wav"}})
	r.SetVoiceprintIDs([]string{"vp-1"})
	r.SetContext(&Context{Text: "bg", Terms: []string{"ASR"}, General: []ContextKV{{Key: "domain", Value: "Meeting"}}})

	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer r.Stop()

	// Handshake URL: path /asr/v3, query carries only voice_id.
	u := s.getHandshakeURL()
	if u == nil {
		t.Fatal("server did not receive handshake")
	}
	if u.Path != "/asr/v3" {
		t.Errorf("path = %q, want /asr/v3", u.Path)
	}
	q := u.Query()
	if q.Get("voice_id") != "voice-1" {
		t.Errorf("voice_id = %q, want voice-1", q.Get("voice_id"))
	}
	for _, k := range []string{"signature", "usersig", "sdkappid", "secretid", "timestamp", "appid"} {
		if _, ok := q[k]; ok {
			t.Errorf("query must not carry %q (v3 moves auth into the start frame)", k)
		}
	}

	// Start frame structure.
	raw := s.getStartFrame()
	if len(raw) == 0 {
		t.Fatal("server did not receive start frame")
	}
	if len(raw) > startFrameMaxBytes {
		t.Errorf("start frame exceeds %d bytes", startFrameMaxBytes)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("start frame is not JSON: %v", err)
	}
	var typ string
	_ = json.Unmarshal(frame["type"], &typ)
	if typ != "start" {
		t.Errorf("type = %q, want start", typ)
	}

	var auth map[string]interface{}
	if err := json.Unmarshal(frame["auth"], &auth); err != nil {
		t.Fatalf("auth block missing: %v", err)
	}
	if auth["sdkappid"] != "1400000000" {
		t.Errorf("auth.sdkappid = %v, want string 1400000000", auth["sdkappid"])
	}
	if auth["usersig"] == "" {
		t.Error("auth.usersig is empty")
	}
	if len(auth) != 2 {
		t.Errorf("auth fields = %v, want only sdkappid and usersig", auth)
	}

	var params map[string]interface{}
	if err := json.Unmarshal(frame["params"], &params); err != nil {
		t.Fatalf("params block missing: %v", err)
	}
	want := map[string]interface{}{
		"voice_id":            "voice-1",
		"engine_model_type":   "16k_zh_en",
		"voice_format":        float64(1),
		"needvad":             float64(1),
		"convert_num_mode":    float64(0), // explicit 0 preserved
		"filter_empty_result": float64(0), // explicit 0 preserved
		"vad_level":           float64(0), // explicit 0 preserved
		"noise_threshold":     1.5,
		"hotword_list":        "深度学习|10",
		"speaker_diarization": float64(3),
		"voiceprint_ids":      []interface{}{"vp-1"},
	}
	for k, v := range want {
		got, ok := params[k]
		if !ok {
			t.Errorf("params.%s missing", k)
			continue
		}
		if !reflect.DeepEqual(got, v) {
			t.Errorf("params.%s = %v (%T), want %v (%T)", k, got, got, v, v)
		}
	}
	// speaker_roles elements are snake_case (role_name / audio_url), not the
	// v2 CamelCase wire.
	roles, ok := params["speaker_roles"].([]interface{})
	if !ok || len(roles) != 1 {
		t.Fatalf("params.speaker_roles = %v", params["speaker_roles"])
	}
	role := roles[0].(map[string]interface{})
	if role["role_name"] != "teacher" || role["audio_url"] != "https://example.com/t.wav" {
		t.Errorf("speaker_roles[0] = %v", role)
	}
	if _, bad := role["RoleName"]; bad {
		t.Error("speaker_roles element must be snake_case, found RoleName")
	}
	// context block passes through.
	ctx, ok := params["context"].(map[string]interface{})
	if !ok || ctx["text"] != "bg" {
		t.Errorf("params.context = %v", params["context"])
	}
	// sdk_info telemetry is present (survives in server dumps).
	info, ok := params["sdk_info"].(map[string]interface{})
	if !ok || info["sdk_lang"] != "go" || info["version"] == "" {
		t.Errorf("params.sdk_info = %v", params["sdk_info"])
	}
}

// TestStartAuthErrorSync verifies v3 fails Start synchronously on a structured
// error frame (instead of reporting it later via OnFail like v2).
func TestStartAuthErrorSync(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, map[string]interface{}{
			"code": 4002, "message": "auth failed", "voice_id": "v1",
		})
		// Server closes normally after the error frame (defer conn.Close).
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())

	err := r.Start()
	if err == nil {
		t.Fatal("Start must fail on 4002")
	}
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) {
		t.Fatalf("error type = %T, want *common.ASRError", err)
	}
	if asrErr.Code != 4002 {
		t.Errorf("error code = %d, want server code 4002", asrErr.Code)
	}
	// A failed Start leaves the recognizer re-startable-in-name only; state is
	// back to idle semantics: Write/Stop reject.
	if werr := r.Write([]byte{0}); werr == nil {
		t.Error("Write after failed Start must fail")
	}
}

// TestNormalFlow drives a full session: ack → audio → end → results → final.
func TestNormalFlow(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(0, 0, ""))    // sentence begin
		writeJSON(t, conn, resultFrame(1, 0, "你好"))  // interim
		writeJSON(t, conn, resultFrame(2, 0, "你好。")) // sentence end
		writeJSON(t, conn, resultFrame(2, 1, "你好。")) // final
	})

	listener := newTestListener()
	r := newRecognizerForTest(t, listener)
	r.SetEndpoint(s.wsURL())

	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := r.Write(make([]byte, 1280)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	waitFor(t, s.gotEnd, "end frame")

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.startN != 1 {
		t.Errorf("OnRecognitionStart = %d, want 1", listener.startN)
	}
	if listener.sentenceN != 1 {
		t.Errorf("OnSentenceBegin = %d, want 1", listener.sentenceN)
	}
	if listener.changeN != 1 {
		t.Errorf("OnRecognitionResultChange = %d, want 1", listener.changeN)
	}
	if listener.endN != 2 { // slice_type=2 and final=1&slice_type=2 both dispatch
		t.Errorf("OnSentenceEnd = %d, want 2", listener.endN)
	}
	if listener.completeN != 1 {
		t.Errorf("OnRecognitionComplete = %d, want 1", listener.completeN)
	}
	if listener.failN != 0 {
		t.Errorf("OnFail = %d, want 0", listener.failN)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.audioFrames) != 1 || len(s.audioFrames[0]) != 1280 {
		t.Errorf("audio frames = %v", s.audioFrames)
	}
}

// TestMidSessionError verifies a code!=0 frame mid-session terminates the
// session and reaches OnFail with the server code.
func TestMidSessionError(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		writeJSON(t, conn, map[string]interface{}{
			"code": 4008, "message": "audio timeout", "voice_id": "v1",
		})
	})

	listener := newTestListener()
	r := newRecognizerForTest(t, listener)
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case ev := <-listener.failCh:
		var asrErr *common.ASRError
		if !errors.As(ev.err, &asrErr) || asrErr.Code != 4008 {
			t.Errorf("OnFail error = %v, want code 4008", ev.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for OnFail")
	}
}

// TestWriteFrameTooLarge verifies the local 256KB audio frame limit.
func TestWriteFrameTooLarge(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer r.Stop()

	if err := r.Write(make([]byte, streamFrameMaxBytes+1)); err == nil {
		t.Error("oversized audio frame must fail locally")
	}
	if err := r.Write(make([]byte, streamFrameMaxBytes)); err != nil {
		t.Errorf("max-size frame must pass: %v", err)
	}
}

// TestAckTimeout verifies Start fails (rather than hanging) when the server
// never answers the start frame.
func TestAckTimeout(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		time.Sleep(7 * time.Second) // never ack within ackTimeout (5s)
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())

	start := time.Now()
	err := r.Start()
	if err == nil {
		t.Fatal("Start must fail when the ack never arrives")
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Errorf("Start blocked %v, want ack timeout ~5s", elapsed)
	}
}

// TestPreResultFrame verifies a result frame arriving together with (before
// the read loop starts) is not dropped: connect stashes it when it carries a
// result, and readLoop dispatches it.
func TestPreResultFrame(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		// Deliberately send a result frame as the very first frame instead of
		// a bare ack (code=0 with result) to exercise the stash path.
		writeJSON(t, conn, resultFrame(1, 0, "早到结果"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	listener := newTestListener()
	r := newRecognizerForTest(t, listener)
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.changeN != 1 {
		t.Errorf("OnRecognitionResultChange = %d, want 1 (stashed pre-result)", listener.changeN)
	}
	if listener.completeN != 1 {
		t.Errorf("OnRecognitionComplete = %d, want 1", listener.completeN)
	}
}

// TestDoubleStartRejected keeps the single-use lifecycle guarantee.
func TestDoubleStartRejected(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if err := r.Start(); err == nil {
		t.Error("second Start must fail")
	}
	if err := r.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

// TestConcurrentWriteStop exercises write/stop concurrency under -race.
func TestConcurrentWriteStop(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var stopN int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = r.Write(make([]byte, 320))
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		if err := r.Stop(); err == nil {
			atomic.AddInt32(&stopN, 1)
		}
	}()
	wg.Wait()
}

// TestStartFrameDefaults pins the tri-state pointer semantics: fields whose
// setters were never called must be absent from params (so the server default
// applies), while SDK-managed defaults (needvad/convert_num_mode) are always
// sent.
func TestStartFrameDefaults(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	r := newRecognizerForTest(t, newTestListener())
	r.SetEndpoint(s.wsURL())
	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer r.Stop()

	raw := s.getStartFrame()
	var frame struct {
		Params map[string]interface{} `json:"params"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("start frame is not JSON: %v", err)
	}
	params := frame.Params

	for _, absent := range []string{
		"vad_level", "noise_threshold", "filter_empty_result",
		"vad_silence_time", "input_sample_rate",
	} {
		if _, ok := params[absent]; ok {
			t.Errorf("params.%s must be absent when its setter was not called (got %v)", absent, params[absent])
		}
	}
	// SDK-managed defaults are always sent.
	for k, want := range map[string]interface{}{
		"needvad":          float64(1),
		"convert_num_mode": float64(1),
		"voice_format":     float64(1),
	} {
		if got := params[k]; !reflect.DeepEqual(got, want) {
			t.Errorf("params.%s = %v, want %v", k, got, want)
		}
	}
	// engine_model_type is required on the wire.
	if params["engine_model_type"] != "16k_zh_en" {
		t.Errorf("params.engine_model_type = %v", params["engine_model_type"])
	}
}

// TestValidationLocal pins the local fast-fail ranges mirroring the server
// validator: invalid options must fail Start before dialing.
func TestValidationLocal(t *testing.T) {
	longVoiceID := strings.Repeat("x", maxVoiceIDLen+1)
	cases := []struct {
		name  string
		apply func(r *SpeechRecognizer)
	}{
		{"voice_id too long", func(r *SpeechRecognizer) { r.SetVoiceID(longVoiceID) }},
		{"max_speak_time too small", func(r *SpeechRecognizer) { r.SetMaxSpeakTime(1000) }},
		{"max_speak_time too large", func(r *SpeechRecognizer) { r.SetMaxSpeakTime(90001) }},
		{"vad_silence_time too small", func(r *SpeechRecognizer) { r.SetVadSilenceTime(100) }},
		{"vad_silence_time too large", func(r *SpeechRecognizer) { r.SetVadSilenceTime(2001) }},
		{"needvad invalid", func(r *SpeechRecognizer) { r.SetNeedVad(2) }},
		{"convert_num_mode invalid", func(r *SpeechRecognizer) { r.SetConvertNumMode(2) }},
		{"filter_dirty invalid", func(r *SpeechRecognizer) { r.SetFilterDirty(3) }},
		{"filter_modal invalid", func(r *SpeechRecognizer) { r.SetFilterModal(3) }},
		{"filter_punc invalid", func(r *SpeechRecognizer) { r.SetFilterPunc(2) }},
		{"word_info invalid", func(r *SpeechRecognizer) { r.SetWordInfo(3) }},
		{"word_with_space invalid", func(r *SpeechRecognizer) { r.SetWordWithSpace(2) }},
		{"voice_format invalid", func(r *SpeechRecognizer) { r.SetVoiceFormat(2) }},
		{"input_sample_rate invalid", func(r *SpeechRecognizer) { r.SetInputSampleRate(16000) }},
		{"filter_empty_result invalid", func(r *SpeechRecognizer) { r.SetFilterEmptyResult(2) }},
		{"vad_level invalid", func(r *SpeechRecognizer) { r.SetVadLevel(2) }},
		{"noise_threshold out of range", func(r *SpeechRecognizer) { r.SetNoiseThreshold(4.1) }},
		{"diarization invalid", func(r *SpeechRecognizer) { r.SetSpeakerDiarization(2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Unreachable endpoint: validation must fail before any dial.
			r := newRecognizerForTest(t, newTestListener())
			r.SetEndpoint("ws://127.0.0.1:1")
			tc.apply(r)
			err := r.Start()
			var asrErr *common.ASRError
			if !errors.As(err, &asrErr) || asrErr.Code != common.ErrCodeInvalidParam {
				t.Errorf("Start error = %v, want ErrCodeInvalidParam", err)
			}
		})
	}

	// Boundary values must pass validation (they then fail at dial, which is
	// expected with an unreachable endpoint).
	valid := []struct {
		name  string
		apply func(r *SpeechRecognizer)
	}{
		{"max_speak_time lower bound", func(r *SpeechRecognizer) { r.SetMaxSpeakTime(5000) }},
		{"max_speak_time upper bound", func(r *SpeechRecognizer) { r.SetMaxSpeakTime(90000) }},
		{"vad_silence_time bounds", func(r *SpeechRecognizer) { r.SetVadSilenceTime(240) }},
		{"vad_silence_time out of range but vad off", func(r *SpeechRecognizer) {
			r.SetNeedVad(0)
			r.SetVadSilenceTime(100) // only validated with needvad=1, same as the server
		}},
		{"voice_format wav", func(r *SpeechRecognizer) { r.SetVoiceFormat(12) }},
		{"word_info caption", func(r *SpeechRecognizer) { r.SetWordInfo(100) }},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecognizerForTest(t, newTestListener())
			r.SetEndpoint("ws://127.0.0.1:1")
			tc.apply(r)
			err := r.Start()
			var asrErr *common.ASRError
			if errors.As(err, &asrErr) && asrErr.Code == common.ErrCodeInvalidParam {
				t.Errorf("valid option rejected locally: %v", err)
			}
		})
	}
}

// stopFromCallbackListener calls Stop re-entrantly inside OnSentenceBegin.
// The recognizer reference is wired after construction (the recognizer needs
// the listener at construction time).
type stopFromCallbackListener struct {
	*testListener
	rec     *SpeechRecognizer
	once    sync.Once
	stopErr error
	stopCh  chan struct{}
}

func (l *stopFromCallbackListener) OnSentenceBegin(resp *SpeechRecognitionResponse) {
	l.testListener.OnSentenceBegin(resp)
	l.once.Do(func() {
		l.stopErr = l.rec.Stop() // re-entrant: must not self-block the read loop
		close(l.stopCh)
	})
}

// TestStopFromListenerCallback verifies Stop is safe to call re-entrantly from
// a recognition callback: it sends the end frame and returns without waiting
// for the terminal response (waiting would self-block the read loop).
func TestStopFromListenerCallback(t *testing.T) {
	s := startV3Server(t, func(conn *websocket.Conn, s *v3Server) {
		writeJSON(t, conn, ackFrame("v1"))
		writeJSON(t, conn, resultFrame(0, 0, "")) // sentence begin → triggers callback Stop
		s.readAudioUntilEnd(conn)
		writeJSON(t, conn, resultFrame(2, 1, "done"))
	})

	listener := &stopFromCallbackListener{
		testListener: newTestListener(),
		stopCh:       make(chan struct{}),
	}
	r := newRecognizerForTest(t, listener)
	listener.rec = r
	r.SetEndpoint(s.wsURL())

	if err := r.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	select {
	case <-listener.stopCh:
		if listener.stopErr != nil {
			t.Errorf("re-entrant Stop returned error: %v", listener.stopErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("re-entrant Stop blocked (self-deadlock)")
	}

	// The session still completes: end reached the server, final comes back.
	select {
	case <-listener.completeCh:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not complete after re-entrant Stop")
	}
	waitFor(t, s.gotEnd, "end frame")

	// A later external Stop observes the stopped state.
	if err := r.Stop(); err == nil {
		t.Error("Stop after completion must report not running")
	}
}
