package asr

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	startCh    chan *SpeechRecognitionResponse
	sentenceCh chan *SpeechRecognitionResponse
	failCh     chan failEvent
	completeCh chan *SpeechRecognitionResponse
}

func newTestListener() *testListener {
	return &testListener{
		startCh:    make(chan *SpeechRecognitionResponse, 8),
		sentenceCh: make(chan *SpeechRecognitionResponse, 8),
		failCh:     make(chan failEvent, 8),
		completeCh: make(chan *SpeechRecognitionResponse, 8),
	}
}

func (l *testListener) OnRecognitionStart(resp *SpeechRecognitionResponse) {
	l.mu.Lock()
	l.startN++
	l.mu.Unlock()
	select {
	case l.startCh <- resp:
	default:
	}
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

func newRecognizerForTest(listener SpeechRecognitionListener) *SpeechRecognizer {
	cred := common.NewCredential(1300000000, 1400000000, "test-secret")
	r := NewSpeechRecognizer(cred, "16k_zh_en", listener)
	r.SetWriteTimeout(200 * time.Millisecond)
	return r
}

func newWSPair(t *testing.T) (clientConn *websocket.Conn, serverConn *websocket.Conn, cleanup func()) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 1)
	serverErrCh := make(chan error, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErrCh <- err
			return
		}
		serverConnCh <- conn
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial websocket failed: %v", err)
	}

	var server *websocket.Conn
	select {
	case server = <-serverConnCh:
	case err = <-serverErrCh:
		_ = client.Close()
		srv.Close()
		t.Fatalf("upgrade websocket failed: %v", err)
	case <-time.After(2 * time.Second):
		_ = client.Close()
		srv.Close()
		t.Fatal("timeout waiting for server websocket connection")
	}

	cleanup = func() {
		if client != nil {
			_ = client.Close()
		}
		if server != nil {
			_ = server.Close()
		}
		srv.Close()
	}
	return client, server, cleanup
}

func expectASRErrorCode(t *testing.T, err error, wantCode int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %d, got nil", wantCode)
	}
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) {
		t.Fatalf("expected *common.ASRError, got %T (%v)", err, err)
	}
	if asrErr.Code != wantCode {
		t.Fatalf("error code = %d, want %d, error=%v", asrErr.Code, wantCode, asrErr)
	}
}

func TestWriteBeforeStartReturnsNotStarted(t *testing.T) {
	r := newRecognizerForTest(newTestListener())

	err := r.Write([]byte("abc"))
	expectASRErrorCode(t, err, common.ErrCodeNotStarted)
}

func TestStopWithNilConnectionReturnsNotStartedAndStops(t *testing.T) {
	r := newRecognizerForTest(newTestListener())
	atomic.StoreInt32(&r.state, stateRunning)
	r.conn = nil

	err := r.Stop()
	expectASRErrorCode(t, err, common.ErrCodeNotStarted)

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d", got, stateStopped)
	}
}

func TestStopSendFailureReturnsWriteFailedAndStops(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, _, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)

	_ = client.Close()

	err := r.Stop()
	expectASRErrorCode(t, err, common.ErrCodeWriteFailed)

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d", got, stateStopped)
	}
}

func TestReadLoopServerErrorTriggersOnFail(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	err := server.WriteMessage(websocket.TextMessage, []byte(`{"code":4006,"message":"quota exceeded","voice_id":"v1","result":{}}`))
	if err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case evt := <-listener.failCh:
		if evt.resp == nil {
			t.Fatal("expected non-nil response in OnFail")
		}
		if evt.resp.Code != 4006 {
			t.Fatalf("resp.Code = %d, want 4006", evt.resp.Code)
		}
		var asrErr *common.ASRError
		if !errors.As(evt.err, &asrErr) {
			t.Fatalf("expected *common.ASRError, got %T", evt.err)
		}
		if asrErr.Code != 4006 {
			t.Fatalf("OnFail error code = %d, want 4006", asrErr.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnFail")
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for doneCh close")
	}
}

func TestReadLoopFinalTriggersOnRecognitionComplete(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2,"index":0,"voice_text_str":"hello"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case resp := <-listener.completeCh:
		if resp == nil {
			t.Fatal("expected non-nil response in OnRecognitionComplete")
		}
		if resp.Final != 1 {
			t.Fatalf("resp.Final = %d, want 1", resp.Final)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete")
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for doneCh close")
	}
}

func TestReadLoopTriggersRecognitionStartBeforeServerEvents(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	r.SetVoiceID("voice-start")
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"voice-start","result":{"slice_type":0,"index":0}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case resp := <-listener.startCh:
		if resp.VoiceID != "voice-start" {
			t.Fatalf("start VoiceID = %q, want voice-start", resp.VoiceID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionStart")
	}

	select {
	case <-listener.sentenceCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnSentenceBegin")
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.startN != 1 {
		t.Fatalf("startN = %d, want 1", listener.startN)
	}
	if listener.sentenceN != 1 {
		t.Fatalf("sentenceN = %d, want 1", listener.sentenceN)
	}
}

// TestReadLoopHandshakeAckDoesNotTriggerSentenceBegin reproduces the real
// server handshake behavior: the first frame after connect is a connection
// acknowledgement that carries no "result" object and no "message_id"
// (e.g. {"code":0,"message":"success","voice_id":"v1"}). Decoding it yields a
// zero-valued Result (SliceType=0), which must NOT be mistaken for a
// slice_type=0 "sentence begin" frame. Only the subsequent real result frame
// should drive OnSentenceBegin.
func TestReadLoopHandshakeAckDoesNotTriggerSentenceBegin(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	// 1. Connection ack: code 0, no result, no message_id.
	ack := `{"code":0,"message":"success","voice_id":"v1"}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(ack)); err != nil {
		t.Fatalf("server write ack failed: %v", err)
	}

	// 2. The one and only real sentence-begin frame.
	begin := `{"code":0,"message":"success","voice_id":"v1","message_id":"m1","result":{"slice_type":0,"index":0,"voice_text_str":"今天。"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(begin)); err != nil {
		t.Fatalf("server write begin failed: %v", err)
	}

	// 3. Final frame ends the session.
	final := `{"code":0,"message":"success","voice_id":"v1","message_id":"m2","final":1}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(final)); err != nil {
		t.Fatalf("server write final failed: %v", err)
	}

	select {
	case <-listener.completeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete")
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.startN != 1 {
		t.Fatalf("startN = %d, want 1", listener.startN)
	}
	if listener.sentenceN != 1 {
		t.Fatalf("sentenceN = %d, want 1 (handshake ack must not trigger sentence begin)", listener.sentenceN)
	}
}

func TestReadLoopFinalWithSliceZeroDoesNotTriggerSentenceBegin(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":0,"index":0}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.completeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete")
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.sentenceN != 0 {
		t.Fatalf("sentenceN = %d, want 0 for final slice_type=0", listener.sentenceN)
	}
	if listener.completeN != 1 {
		t.Fatalf("completeN = %d, want 1", listener.completeN)
	}
}

func TestConcurrentStopAndReadLoopCloseNoDeadlock(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_, _, err := server.ReadMessage()
		if err != nil {
			return
		}
		_ = server.WriteMessage(websocket.TextMessage, []byte(`{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`))
		_ = server.Close()
	}()

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- r.Stop()
	}()

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for Stop (possible deadlock)")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server goroutine")
	}

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d", got, stateStopped)
	}
}

// TestReadLoopFinalAdvancesStateToStopped verifies that when the server ends
// the session (Final=1) without the caller invoking Stop, the recognizer's
// state is advanced to stopped so that a late Write reports "not running"
// instead of acting on the half-closed connection.
func TestReadLoopFinalAdvancesStateToStopped(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2,"voice_text_str":"done"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for doneCh close")
	}

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d (stopped) after server Final", got, stateStopped)
	}

	if err := r.Write([]byte("late")); err == nil {
		t.Fatal("expected Write to fail after session ended")
	} else {
		expectASRErrorCode(t, err, common.ErrCodeNotStarted)
	}
}

// TestReadLoopServerErrorAdvancesStateToStopped verifies that a server error
// (code != 0) also advances the lifecycle to stopped on readLoop exit.
func TestReadLoopServerErrorAdvancesStateToStopped(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	if err := server.WriteMessage(websocket.TextMessage, []byte(`{"code":4006,"message":"quota exceeded","voice_id":"v1","result":{}}`)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.failCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnFail")
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for doneCh close")
	}

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d (stopped) after server error", got, stateStopped)
	}
}

// panicListener panics inside a recognition callback to simulate a buggy
// user-supplied listener.
type panicListener struct {
	*testListener
}

func (l *panicListener) OnRecognitionResultChange(_ *SpeechRecognitionResponse) {
	panic("listener boom")
}

// TestReadLoopRecoversFromListenerPanic verifies that a panic in a user
// callback does not crash the host process: readLoop recovers, surfaces the
// panic via OnFail, closes doneCh, and advances the state to stopped. Without
// the recover guard this test would crash the whole test binary.
func TestReadLoopRecoversFromListenerPanic(t *testing.T) {
	base := newTestListener()
	listener := &panicListener{testListener: base}
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	// slice_type=1 dispatches to OnRecognitionResultChange, which panics.
	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"hi"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case evt := <-base.failCh:
		var asrErr *common.ASRError
		if !errors.As(evt.err, &asrErr) {
			t.Fatalf("expected *common.ASRError, got %T", evt.err)
		}
		errMsg := asrErr.Error()
		if !strings.Contains(errMsg, "panic") {
			t.Fatalf("expected panic info in OnFail error, got %v", errMsg)
		}
		// Verify the error includes a stack trace so the caller can pinpoint
		// the panic origin. runtime/debug.Stack produces output that starts
		// with "goroutine" and contains function/file/line information.
		if !strings.Contains(errMsg, "goroutine") {
			t.Fatalf("expected stack trace (goroutine) in OnFail error, got %v", errMsg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnFail after panic")
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for doneCh close after panic")
	}

	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d (stopped) after panic", got, stateStopped)
	}
}

// panicInCallbackStopListener panics inside a callback and then calls Stop()
// from OnFail (triggered by the recover in readLoop's defer). This exercises
// the re-entrancy guarantee: Stop() must return immediately, not deadlock for
// 10s waiting on doneCh.
type panicInCallbackStopListener struct {
	*testListener
	r            *SpeechRecognizer
	stopErr      error
	stopDur      time.Duration
	stopCalledCh chan struct{}
}

func (l *panicInCallbackStopListener) OnRecognitionResultChange(_ *SpeechRecognitionResponse) {
	panic("listener boom in change")
}

func (l *panicInCallbackStopListener) OnFail(resp *SpeechRecognitionResponse, err error) {
	l.testListener.OnFail(resp, err)
	start := time.Now()
	l.stopErr = l.r.Stop()
	l.stopDur = time.Since(start)
	close(l.stopCalledCh)
}

// TestStopFromPanicRecoverOnFailReturnsImmediately verifies that calling Stop
// from inside the OnFail callback triggered by a panic recover does not block
// for the 10s server-wait timeout. When a listener callback panics, readLoop
// must finish the terminal lifecycle before surfacing the recovered panic via
// OnFail; otherwise Stop would CAS successfully and then block on doneCh —
// which only finish() closes — causing a 10s self-deadlock.
func TestStopFromPanicRecoverOnFailReturnsImmediately(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &panicInCallbackStopListener{
		testListener: base,
		r:            r,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	// slice_type=1 dispatches to OnRecognitionResultChange, which panics.
	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"hi"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() inside panic-recover OnFail callback did not return promptly (possible 10s self-deadlock)")
	}

	if listener.stopDur > 2*time.Second {
		t.Fatalf("Stop() inside panic-recover OnFail took %v, want prompt return", listener.stopDur)
	}
	expectASRErrorCode(t, listener.stopErr, common.ErrCodeNotStarted)
}

// sentenceEndStopListener calls Stop() from within OnSentenceEnd to exercise
// the re-entrancy guarantee when the callback fires on a terminal response
// (Final=1, slice_type=2). readLoop must finish before dispatching that
// terminal OnSentenceEnd; otherwise Stop would CAS successfully and then block
// on doneCh — causing a 10s self-deadlock.
type sentenceEndStopListener struct {
	*testListener
	r            *SpeechRecognizer
	stopErr      error
	stopDur      time.Duration
	stopCalledCh chan struct{}
}

func (l *sentenceEndStopListener) OnSentenceEnd(_ *SpeechRecognitionResponse) {
	l.testListener.OnSentenceEnd(nil)
	start := time.Now()
	l.stopErr = l.r.Stop()
	l.stopDur = time.Since(start)
	close(l.stopCalledCh)
}

// changeStopListener calls Stop() from within OnRecognitionResultChange
// (a non-terminal callback) to verify it does not self-wait for doneCh.
type changeStopListener struct {
	*testListener
	r            *SpeechRecognizer
	stopErr      error
	stopDur      time.Duration
	stopCalledCh chan struct{}
}

type blockingChangeListener struct {
	*testListener
	enteredCh chan struct{}
	releaseCh chan struct{}
}

func (l *changeStopListener) OnRecognitionResultChange(_ *SpeechRecognitionResponse) {
	l.testListener.OnRecognitionResultChange(nil)
	start := time.Now()
	l.stopErr = l.r.Stop()
	l.stopDur = time.Since(start)
	close(l.stopCalledCh)
}

func (l *blockingChangeListener) OnRecognitionResultChange(resp *SpeechRecognitionResponse) {
	l.testListener.OnRecognitionResultChange(resp)
	close(l.enteredCh)
	<-l.releaseCh
}

// TestStopFromOnSentenceEndOnFinalResponseReturnsImmediately verifies that
// calling Stop from inside OnSentenceEnd — when the response is a terminal
// one (Final=1, slice_type=2) — returns immediately rather than blocking for
// the 10s server-wait timeout. readLoop should finish before dispatching this
// terminal OnSentenceEnd, so Stop sees the stopped state and returns promptly.
func TestStopFromOnSentenceEndOnFinalResponseReturnsImmediately(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &sentenceEndStopListener{
		testListener: base,
		r:            r,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	// Final=1 with slice_type=2: readLoop must finish before dispatching
	// OnSentenceEnd, then call OnRecognitionComplete.
	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2,"index":0,"voice_text_str":"done"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() inside OnSentenceEnd (Final=1, slice_type=2) did not return promptly (possible 10s self-deadlock)")
	}

	if listener.stopDur > 2*time.Second {
		t.Fatalf("Stop() inside OnSentenceEnd took %v, want prompt return", listener.stopDur)
	}
	expectASRErrorCode(t, listener.stopErr, common.ErrCodeNotStarted)
}

// TestStopFromResultChangeReturnsPromptly verifies Stop called from a
// non-terminal callback returns after sending end instead of waiting for doneCh
// on the same readLoop goroutine (which would otherwise time out at ~10s).
func TestStopFromResultChangeReturnsPromptly(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &changeStopListener{
		testListener: base,
		r:            r,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"partial"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() inside OnRecognitionResultChange did not return promptly")
	}

	if listener.stopDur > 2*time.Second {
		t.Fatalf("Stop() inside OnRecognitionResultChange took %v, want prompt return", listener.stopDur)
	}
	if listener.stopErr != nil {
		t.Fatalf("Stop() inside OnRecognitionResultChange returned error: %v", listener.stopErr)
	}
}

// TestStopFromResultChangeTimesOutIfServerNeverFinishes verifies that callback
// re-entrant Stop does not leave readLoop blocked forever when the server reads
// end but never returns a terminal response.
func TestStopFromResultChangeTimesOutIfServerNeverFinishes(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	r.stopTimeout = 50 * time.Millisecond
	listener := &changeStopListener{
		testListener: base,
		r:            r,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"partial"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() inside OnRecognitionResultChange did not return promptly")
	}
	if listener.stopErr != nil {
		t.Fatalf("Stop() inside OnRecognitionResultChange returned error: %v", listener.stopErr)
	}

	if _, _, err := server.ReadMessage(); err != nil {
		t.Fatalf("server read end signal failed: %v", err)
	}

	select {
	case <-r.doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for watchdog to close readLoop")
	}
	if got := atomic.LoadInt32(&r.state); got != stateStopped {
		t.Fatalf("state = %d, want %d", got, stateStopped)
	}
}

type deepStopListener struct {
	*testListener
	r            *SpeechRecognizer
	stopErr      error
	stopDur      time.Duration
	stopCalledCh chan struct{}
}

func (l *deepStopListener) OnRecognitionResultChange(_ *SpeechRecognitionResponse) {
	l.callStopDeep(48)
}

func (l *deepStopListener) callStopDeep(depth int) {
	if depth > 0 {
		l.callStopDeep(depth - 1)
		return
	}
	start := time.Now()
	l.stopErr = l.r.Stop()
	l.stopDur = time.Since(start)
	close(l.stopCalledCh)
}

// TestStopFromDeepCallbackStackReturnsPromptly verifies callback detection is
// not truncated by a modest user helper call chain before Stop is invoked.
func TestStopFromDeepCallbackStackReturnsPromptly(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &deepStopListener{
		testListener: base,
		r:            r,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"partial"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop() from deep callback stack did not return promptly")
	}
	if listener.stopDur > 2*time.Second {
		t.Fatalf("Stop() from deep callback stack took %v, want prompt return", listener.stopDur)
	}
	if listener.stopErr != nil {
		t.Fatalf("Stop() from deep callback stack returned error: %v", listener.stopErr)
	}
}

// TestExternalStopDuringCallbackWaitsForCompletion verifies that inCallback is
// not treated as a process-wide shortcut. Stop called by a caller-owned
// goroutine while readLoop is inside a listener callback must still wait for
// the final response and for readLoop to finish.
func TestExternalStopDuringCallbackWaitsForCompletion(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &blockingChangeListener{
		testListener: base,
		enteredCh:    make(chan struct{}),
		releaseCh:    make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	msg := `{"code":0,"message":"ok","voice_id":"v1","result":{"slice_type":1,"index":0,"voice_text_str":"partial"}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write partial failed: %v", err)
	}

	select {
	case <-listener.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionResultChange to start")
	}

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- r.Stop()
	}()

	_, endMsg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("server read end signal failed: %v", err)
	}
	if string(endMsg) != `{"type":"end"}` {
		t.Fatalf("end message = %s, want {\"type\":\"end\"}", endMsg)
	}

	select {
	case err := <-stopErrCh:
		t.Fatalf("Stop returned while callback was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(listener.releaseCh)

	finalMsg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(finalMsg)); err != nil {
		t.Fatalf("server write final failed: %v", err)
	}

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Stop after final response")
	}
}

// TestWriteRejectedWhenStateLeavesRunningUnderWriteMu verifies the double-check
// inside Write: if the state stops being running between the entry check and
// acquiring writeMu (e.g. Stop transitioned the state and sent end), Write must
// bail out instead of sending audio after the end signal.
func TestWriteRejectedWhenStateLeavesRunningUnderWriteMu(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)

	// Simulate a writer in flight by holding writeMu, so Write blocks right
	// after passing its entry state check.
	r.writeMu.Lock()

	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- r.Write([]byte("audio-after-end"))
	}()

	// Let the Write goroutine pass the entry check and block on writeMu.
	time.Sleep(100 * time.Millisecond)

	// Simulate Stop having transitioned the state (and conceptually sent end).
	atomic.StoreInt32(&r.state, stateStopping)

	// Release the writer; Write must now observe the non-running state and bail.
	r.writeMu.Unlock()

	select {
	case err := <-writeErrCh:
		expectASRErrorCode(t, err, common.ErrCodeNotStarted)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Write to return")
	}

	// The server must not have received any audio frame.
	_ = server.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := server.ReadMessage(); err == nil {
		t.Fatal("server unexpectedly received a message after end")
	}
}

// TestStopReturnsNilIfFinalArrivesBeforeEndSignalWrite verifies Stop does not
// report a write failure when the session naturally finishes while Stop is
// waiting for an in-flight writer to release writeMu.
func TestStopReturnsNilIfFinalArrivesBeforeEndSignalWrite(t *testing.T) {
	listener := newTestListener()
	r := newRecognizerForTest(listener)
	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	r.writeMu.Lock()
	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- r.Stop()
	}()

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write final failed: %v", err)
	}

	select {
	case <-listener.completeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete")
	}

	r.writeMu.Unlock()

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop returned error after natural final: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Stop")
	}
}

// reentrantStopListener calls Stop() from within a terminal callback to exercise
// the re-entrancy guarantee (no 10s self-deadlock).
type reentrantStopListener struct {
	*testListener
	r            *SpeechRecognizer
	on           string // "complete" or "fail"
	stopErr      error
	stopDur      time.Duration
	stopCalledCh chan struct{}
}

func (l *reentrantStopListener) OnRecognitionComplete(resp *SpeechRecognitionResponse) {
	l.testListener.OnRecognitionComplete(resp)
	if l.on == "complete" {
		start := time.Now()
		l.stopErr = l.r.Stop()
		l.stopDur = time.Since(start)
		close(l.stopCalledCh)
	}
}

func (l *reentrantStopListener) OnFail(resp *SpeechRecognitionResponse, err error) {
	l.testListener.OnFail(resp, err)
	if l.on == "fail" {
		start := time.Now()
		l.stopErr = l.r.Stop()
		l.stopDur = time.Since(start)
		close(l.stopCalledCh)
	}
}

func runReentrantStopTest(t *testing.T, on, serverMsg string) {
	t.Helper()
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &reentrantStopListener{
		testListener: base,
		r:            r,
		on:           on,
		stopCalledCh: make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	if err := server.WriteMessage(websocket.TextMessage, []byte(serverMsg)); err != nil {
		t.Fatalf("server write failed: %v", err)
	}

	select {
	case <-listener.stopCalledCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("Stop() inside %s callback did not return promptly (possible 10s self-deadlock)", on)
	}

	if listener.stopDur > 2*time.Second {
		t.Fatalf("Stop() inside %s callback took %v, want prompt return", on, listener.stopDur)
	}
	// The terminal lifecycle already ran before the callback, so a re-entrant
	// Stop is a no-op CAS that reports not-running.
	expectASRErrorCode(t, listener.stopErr, common.ErrCodeNotStarted)
}

// TestStopFromCompleteCallbackReturnsImmediately verifies that calling Stop from
// inside OnRecognitionComplete does not block for the 10s server-wait timeout.
func TestStopFromCompleteCallbackReturnsImmediately(t *testing.T) {
	runReentrantStopTest(t, "complete",
		`{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`)
}

type blockingCompleteListener struct {
	*testListener
	enteredCh chan struct{}
	releaseCh chan struct{}
}

func (l *blockingCompleteListener) OnRecognitionComplete(resp *SpeechRecognitionResponse) {
	l.testListener.OnRecognitionComplete(resp)
	close(l.enteredCh)
	<-l.releaseCh
}

// TestExternalStopWaitsForTerminalCallbacks verifies that Stop called from a
// caller-owned goroutine does not return before the final response callbacks
// have finished. SDK users commonly release resources after Stop returns; if a
// terminal callback is still running at that point, the streaming lifecycle is
// observable out of order.
func TestExternalStopWaitsForTerminalCallbacks(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	listener := &blockingCompleteListener{
		testListener: base,
		enteredCh:    make(chan struct{}),
		releaseCh:    make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- r.Stop()
	}()

	_, endMsg, err := server.ReadMessage()
	if err != nil {
		t.Fatalf("server read end signal failed: %v", err)
	}
	if string(endMsg) != `{"type":"end"}` {
		t.Fatalf("end message = %s, want {\"type\":\"end\"}", endMsg)
	}

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write final failed: %v", err)
	}

	select {
	case <-listener.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete to start")
	}

	select {
	case err := <-stopErrCh:
		t.Fatalf("Stop returned before terminal callback finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(listener.releaseCh)

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Stop after terminal callback released")
	}
}

// TestExternalStopWaitsPastTimeoutOnceTerminalCallbackStarted verifies that
// stopTimeout only bounds waiting for the server terminal response. Once that
// response has arrived and the terminal callback is running, external Stop must
// wait for readLoop to finish delivering it.
func TestExternalStopWaitsPastTimeoutOnceTerminalCallbackStarted(t *testing.T) {
	base := newTestListener()
	r := newRecognizerForTest(base)
	r.stopTimeout = 50 * time.Millisecond
	listener := &blockingCompleteListener{
		testListener: base,
		enteredCh:    make(chan struct{}),
		releaseCh:    make(chan struct{}),
	}
	r.listener = listener

	client, server, cleanup := newWSPair(t)
	defer cleanup()

	r.conn = client
	atomic.StoreInt32(&r.state, stateRunning)
	go r.readLoop()

	stopErrCh := make(chan error, 1)
	go func() {
		stopErrCh <- r.Stop()
	}()

	if _, _, err := server.ReadMessage(); err != nil {
		t.Fatalf("server read end signal failed: %v", err)
	}

	msg := `{"code":0,"message":"ok","voice_id":"v1","final":1,"result":{"slice_type":2}}`
	if err := server.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("server write final failed: %v", err)
	}

	select {
	case <-listener.enteredCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for OnRecognitionComplete to start")
	}

	select {
	case err := <-stopErrCh:
		t.Fatalf("Stop returned while terminal callback was still running: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(listener.releaseCh)

	select {
	case err := <-stopErrCh:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Stop after terminal callback released")
	}
}

// TestStopFromFailCallbackReturnsImmediately verifies the same for OnFail.
func TestStopFromFailCallbackReturnsImmediately(t *testing.T) {
	runReentrantStopTest(t, "fail",
		`{"code":4006,"message":"quota exceeded","voice_id":"v1","result":{}}`)
}

// TestSetWriteTimeoutClamps verifies SetWriteTimeout keeps the value within
// bounds so Stop's worst-case wait to acquire the writer stays predictable.
func TestSetWriteTimeoutClamps(t *testing.T) {
	r := newRecognizerForTest(newTestListener())

	r.SetWriteTimeout(time.Hour)
	if r.writeTimeout != maxWriteTimeout {
		t.Fatalf("writeTimeout = %v, want clamped to %v", r.writeTimeout, maxWriteTimeout)
	}

	r.SetWriteTimeout(time.Nanosecond)
	if r.writeTimeout != minWriteTimeout {
		t.Fatalf("writeTimeout = %v, want clamped to %v", r.writeTimeout, minWriteTimeout)
	}

	r.SetWriteTimeout(-1)
	if r.writeTimeout != defaultWriteTimeout {
		t.Fatalf("writeTimeout = %v, want reset to %v", r.writeTimeout, defaultWriteTimeout)
	}

	r.SetWriteTimeout(2 * time.Second)
	if r.writeTimeout != 2*time.Second {
		t.Fatalf("writeTimeout = %v, want 2s (within bounds, unchanged)", r.writeTimeout)
	}
}

// TestSetStopTimeoutClamps verifies SetStopTimeout keeps the value within
// bounds so Stop's wait for the server's final response stays predictable.
func TestSetStopTimeoutClamps(t *testing.T) {
	r := newRecognizerForTest(newTestListener())

	r.SetStopTimeout(time.Hour)
	if r.stopTimeout != maxStopTimeout {
		t.Fatalf("stopTimeout = %v, want clamped to %v", r.stopTimeout, maxStopTimeout)
	}

	r.SetStopTimeout(time.Millisecond)
	if r.stopTimeout != minStopTimeout {
		t.Fatalf("stopTimeout = %v, want clamped to %v", r.stopTimeout, minStopTimeout)
	}

	r.SetStopTimeout(-1)
	if r.stopTimeout != defaultStopTimeout {
		t.Fatalf("stopTimeout = %v, want reset to %v", r.stopTimeout, defaultStopTimeout)
	}

	r.SetStopTimeout(5 * time.Second)
	if r.stopTimeout != 5*time.Second {
		t.Fatalf("stopTimeout = %v, want 5s (within bounds, unchanged)", r.stopTimeout)
	}
}

type partialListener struct {
	UnimplementedSpeechRecognitionListener
	endN int
}

func (l *partialListener) OnSentenceEnd(resp *SpeechRecognitionResponse) {
	if resp != nil {
		l.endN++
	}
}

func TestPartialListenerEmbeddingIsEnough(t *testing.T) {
	l := &partialListener{}
	r := newRecognizerForTest(l)

	r.listener.OnRecognitionStart(&SpeechRecognitionResponse{})
	r.listener.OnSentenceBegin(&SpeechRecognitionResponse{})
	r.listener.OnRecognitionResultChange(&SpeechRecognitionResponse{})
	r.listener.OnRecognitionComplete(&SpeechRecognitionResponse{})
	r.listener.OnFail(nil, errors.New("ignored"))
	r.listener.OnSentenceEnd(&SpeechRecognitionResponse{})

	if l.endN != 1 {
		t.Fatalf("OnSentenceEnd calls = %d, want 1", l.endN)
	}
}

func TestNilListenerIsReplacedWithNoop(t *testing.T) {
	cred := common.NewCredential(1300000000, 1400000000, "test-secret")
	r := NewSpeechRecognizer(cred, "16k_zh_en", nil)
	if r.listener == nil {
		t.Fatal("nil listener should be replaced with a no-op implementation")
	}

	r.listener.OnRecognitionStart(&SpeechRecognitionResponse{})
	r.listener.OnSentenceBegin(&SpeechRecognitionResponse{})
	r.listener.OnRecognitionResultChange(&SpeechRecognitionResponse{})
	r.listener.OnSentenceEnd(&SpeechRecognitionResponse{})
	r.listener.OnRecognitionComplete(&SpeechRecognitionResponse{})
	r.listener.OnFail(nil, errors.New("ignored"))
}
