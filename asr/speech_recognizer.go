// Package asr provides the TRTC-ASR real-time speech recognition client.
//
// Usage:
//
//	credential := common.NewCredential(appID, sdkAppID, secretKey)
//	listener := &MyListener{}
//	recognizer := asr.NewSpeechRecognizer(credential, engineModelType, listener)
//	recognizer.Start()
//	recognizer.Write(audioData)
//	recognizer.Stop()
package asr

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Endpoint is the production WebSocket endpoint for the TRTC-ASR service.
const Endpoint = "wss://asr.cloud-rtc.com"

// Recognizer states.
const (
	stateIdle     int32 = 0
	stateStarting int32 = 1
	stateRunning  int32 = 2
	stateStopping int32 = 3
	stateStopped  int32 = 4
)

// Write-timeout bounds. A single Write holds the writer for at most
// writeTimeout (enforced via SetWriteDeadline), so Stop's worst-case wait to
// acquire the writer for the end signal is bounded by writeTimeout. Clamping
// keeps Stop's exit time predictable.
const (
	defaultWriteTimeout = 5 * time.Second
	minWriteTimeout     = 50 * time.Millisecond
	maxWriteTimeout     = 30 * time.Second

	// Stop-timeout bounds. stopTimeout caps how long Stop waits for the
	// server's final response after the end signal before forcing the
	// connection closed.
	defaultStopTimeout = 10 * time.Second
	minStopTimeout     = 1 * time.Second
	maxStopTimeout     = 60 * time.Second
)

// SpeechRecognitionListener defines the callback interface for speech recognition events.
//
// Callers that only care about a subset of events can embed
// UnimplementedSpeechRecognitionListener and override the methods they need.
type SpeechRecognitionListener interface {
	// OnRecognitionStart is called when the recognition session starts successfully.
	OnRecognitionStart(response *SpeechRecognitionResponse)
	// OnSentenceBegin is called when a new sentence begins.
	OnSentenceBegin(response *SpeechRecognitionResponse)
	// OnRecognitionResultChange is called when intermediate recognition results are available.
	OnRecognitionResultChange(response *SpeechRecognitionResponse)
	// OnSentenceEnd is called when a sentence ends with the final result.
	OnSentenceEnd(response *SpeechRecognitionResponse)
	// OnRecognitionComplete is called when the entire recognition session completes.
	OnRecognitionComplete(response *SpeechRecognitionResponse)
	// OnFail is called when an error occurs during recognition.
	OnFail(response *SpeechRecognitionResponse, err error)
}

// UnimplementedSpeechRecognitionListener is a no-op SpeechRecognitionListener.
// Embed it and override only the events you care about:
//
//	type MyListener struct {
//	    asr.UnimplementedSpeechRecognitionListener
//	}
//	func (l *MyListener) OnSentenceEnd(resp *SpeechRecognitionResponse) { ... }
type UnimplementedSpeechRecognitionListener struct{}

func (UnimplementedSpeechRecognitionListener) OnRecognitionStart(*SpeechRecognitionResponse)        {}
func (UnimplementedSpeechRecognitionListener) OnSentenceBegin(*SpeechRecognitionResponse)           {}
func (UnimplementedSpeechRecognitionListener) OnRecognitionResultChange(*SpeechRecognitionResponse) {}
func (UnimplementedSpeechRecognitionListener) OnSentenceEnd(*SpeechRecognitionResponse)             {}
func (UnimplementedSpeechRecognitionListener) OnRecognitionComplete(*SpeechRecognitionResponse)     {}
func (UnimplementedSpeechRecognitionListener) OnFail(*SpeechRecognitionResponse, error)             {}

var _ SpeechRecognitionListener = UnimplementedSpeechRecognitionListener{}
var _ SpeechRecognitionListener = (*UnimplementedSpeechRecognitionListener)(nil)

// SpeechRecognitionResponse represents a response message from the ASR service.
type SpeechRecognitionResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	VoiceID   string `json:"voice_id"`
	MessageID string `json:"message_id"`
	Final     int    `json:"final"`
	Result    Result `json:"result"`
}

// Result contains the speech recognition result details.
type Result struct {
	SliceType    int        `json:"slice_type"`
	Index        int        `json:"index"`
	StartTime    int        `json:"start_time"`
	EndTime      int        `json:"end_time"`
	VoiceTextStr string     `json:"voice_text_str"`
	WordSize     int        `json:"word_size"`
	WordList     []WordInfo `json:"word_list"`
	Language     string     `json:"language"` // detected language (bigmodel engine, e.g. "Malay")

	// SpeakerSegments lists the speaker attribution of this result, split by
	// speaker turn. It is the recommended entry point for speaker diarization:
	// one result may contain several speakers, so a sentence-level speaker is
	// ambiguous by design. Empty when diarization is disabled.
	//
	// A result is single-speaker when len(SpeakerSegments) == 1.
	SpeakerSegments []SpeakerSegment `json:"speaker_segments,omitempty"`

	// SpeakerID is the legacy sentence-level speaker attribution. It is a
	// pointer because 0 is a reserved value and the field is absent on most
	// engines. Prefer SpeakerSegments / WordInfo.SpeakerID.
	SpeakerID *int `json:"speaker_id,omitempty"`

	// FinishSilenceMs is the trailing silence (ms) that triggered the sentence
	// break. Zero when the server does not report it.
	FinishSilenceMs int `json:"finish_silence_ms,omitempty"`

	// LastTokenRuntimeMs is the server-side decoding time (ms) of the last
	// token. Zero when the server does not report it.
	LastTokenRuntimeMs int `json:"last_token_runtime_ms,omitempty"`
}

// SpeakerSegment is a contiguous section of one result attributed to a single
// speaker. Returned when speaker diarization is enabled.
type SpeakerSegment struct {
	// SpeakerID is the speaker number within the current session. Valid IDs
	// start at 1, -1 means unknown, 0 is reserved.
	SpeakerID int `json:"speaker_id"`

	// SpeakerName is the enrolled role name, returned only with
	// speaker_diarization=3. It equals the requested SpeakerRole.RoleName.
	SpeakerName string `json:"speaker_name,omitempty"`

	StartTime int    `json:"start_time"`
	EndTime   int    `json:"end_time"`
	Text      string `json:"text,omitempty"`

	// WordStart / WordEnd are inclusive indexes into Result.WordList, i.e.
	// WordList[WordStart : WordEnd+1]. Both are nil when word_info=0 (no
	// word list to index into); 0 is a valid index, hence the pointers.
	WordStart *int `json:"word_start,omitempty"`
	WordEnd   *int `json:"word_end,omitempty"`

	// StableFlag reports whether this segment is stable: 1=stable, 0=not.
	StableFlag int `json:"stable_flag"`
}

// WordInfo contains word-level recognition details.
type WordInfo struct {
	Word       string `json:"word"`
	StartTime  int    `json:"start_time"`
	EndTime    int    `json:"end_time"`
	StableFlag int    `json:"stable_flag"`

	// SpeakerID is the speaker of this word, filled when speaker diarization
	// is enabled together with word_info != 0. Valid IDs start at 1, -1 means
	// unknown, 0 means absent.
	SpeakerID int `json:"speaker_id,omitempty"`

	// SpeakerName is the enrolled role name, returned only with
	// speaker_diarization=3.
	SpeakerName string `json:"speaker_name,omitempty"`
}

// SpeakerRole is a temporary voiceprint enrollment entry for
// speaker_diarization=3. RoleName is echoed back as SpeakerName on matched
// words and speaker segments.
type SpeakerRole = common.SpeakerRole

// Speaker diarization modes accepted by SetSpeakerDiarization.
const (
	// SpeakerDiarizationOff disables speaker diarization (default).
	SpeakerDiarizationOff = common.SpeakerDiarizationOff
	// SpeakerDiarizationCluster enables anonymous speaker clustering.
	SpeakerDiarizationCluster = common.SpeakerDiarizationCluster
	// SpeakerDiarizationVoiceprint enables voiceprint role authentication.
	SpeakerDiarizationVoiceprint = common.SpeakerDiarizationVoiceprint
)

// SpeechRecognizer is the main client for real-time speech recognition.
//
// Lifecycle and concurrency:
//   - A SpeechRecognizer is single-use: once it reaches the stopped state (via
//     Stop or a terminal error) it cannot be restarted. Create a new instance
//     to reconnect.
//   - All SetXxx options must be configured before Start and must not be called
//     concurrently with Start.
//   - After Start returns, Write and Stop may be called from a goroutine other
//     than the one that called Start. Recognition callbacks are delivered on an
//     internal goroutine.
type SpeechRecognizer struct {
	credential *common.Credential
	listener   SpeechRecognitionListener
	conn       *websocket.Conn

	// Configuration
	endpoint           string
	engineModelType    string
	voiceFormat        int
	needVad            int
	convertNumMode     int
	hotwordID          string
	hotwordList        string
	customizationID    string
	replaceTextID      string
	filterDirty        int
	filterModal        int
	filterPunc         int
	filterEmptyResult  *int
	wordInfo           int
	vadSilenceTime     int
	vadLevel           *int
	noiseThreshold     *float64
	maxSpeakTime       int
	inputSampleRate    int
	speakerDiarization int
	speakerNumber      int
	speakerRoles       []SpeakerRole
	voiceprintIDs      []string
	voiceID            string
	language           string // bigmodel engine language hint

	// State management.
	//
	// mu guards only the conn field; its critical sections are short and never
	// contain network I/O, so close() can always acquire it and shut the
	// connection down promptly — even while a Write is blocked on the network.
	//
	// writeMu serializes WebSocket writes (Write and Stop's end signal).
	// gorilla/websocket permits at most one concurrent writer per connection.
	state      int32
	mu         sync.Mutex
	writeMu    sync.Mutex
	doneCh     chan struct{}
	terminalCh chan struct{}
	finishOnce sync.Once
	doneOnce   sync.Once
	termOnce   sync.Once

	// Timeouts
	writeTimeout time.Duration
	stopTimeout  time.Duration
}

// NewSpeechRecognizer creates a new SpeechRecognizer instance.
//
// Parameters:
//   - credential: TRTC authentication credential
//   - engineModelType: recognition engine model (e.g., "16k_zh", "8k_zh", "16k_zh_en")
//   - listener: callback listener for recognition events. A nil listener is
//     replaced with UnimplementedSpeechRecognitionListener so the SDK never
//     panics on a missing callback.
func NewSpeechRecognizer(
	credential *common.Credential,
	engineModelType string,
	listener SpeechRecognitionListener,
) *SpeechRecognizer {
	if listener == nil {
		listener = UnimplementedSpeechRecognitionListener{}
	}
	return &SpeechRecognizer{
		credential:      credential,
		listener:        listener,
		endpoint:        "",
		engineModelType: engineModelType,
		voiceFormat:     1, // PCM
		needVad:         1,
		convertNumMode:  1,
		writeTimeout:    defaultWriteTimeout,
		stopTimeout:     defaultStopTimeout,
		doneCh:          make(chan struct{}),
		terminalCh:      make(chan struct{}),
	}
}

// SetVoiceFormat sets the audio encoding format.
// 1: PCM (default). Other values depend on the formats supported by the engine.
func (r *SpeechRecognizer) SetVoiceFormat(format int) {
	r.voiceFormat = format
}

// SetNeedVad sets whether to enable VAD (Voice Activity Detection).
// 0: disable, 1: enable (default)
func (r *SpeechRecognizer) SetNeedVad(needVad int) {
	r.needVad = needVad
}

// SetConvertNumMode sets the number conversion mode.
// 0: no conversion, 1: smart conversion (default), 3: math conversion
func (r *SpeechRecognizer) SetConvertNumMode(mode int) {
	r.convertNumMode = mode
}

// SetHotwordID sets the hotword list ID for biasing recognition.
func (r *SpeechRecognizer) SetHotwordID(id string) {
	r.hotwordID = id
}

// SetHotwordList sets a temporary inline hotword list, which does not require
// creating a hotword table on the console.
//
// Format: "word1|weight1,word2|weight2". Each word is at most 30 bytes and the
// weight must be 1-11 (11 = super hotword) or 100 (homophone replacement).
func (r *SpeechRecognizer) SetHotwordList(list string) {
	r.hotwordList = list
}

// SetCustomizationID sets the custom language model ID.
func (r *SpeechRecognizer) SetCustomizationID(id string) {
	r.customizationID = id
}

// SetReplaceTextID sets the replacement word table ID used for forced text
// replacement on the recognized result.
func (r *SpeechRecognizer) SetReplaceTextID(id string) {
	r.replaceTextID = id
}

// SetFilterDirty sets the profanity filter mode.
// 0: no filter (default), 1: filter, 2: replace with *
func (r *SpeechRecognizer) SetFilterDirty(mode int) {
	r.filterDirty = mode
}

// SetFilterModal sets the modal particle filter mode.
// 0: no filter (default), 1: partial filter, 2: strict filter
func (r *SpeechRecognizer) SetFilterModal(mode int) {
	r.filterModal = mode
}

// SetFilterPunc sets the sentence-ending punctuation filter mode.
// 0: no filter (default), 1: filter
func (r *SpeechRecognizer) SetFilterPunc(mode int) {
	r.filterPunc = mode
}

// SetFilterEmptyResult sets whether empty recognition results are delivered.
// 0: deliver empty results, 1: skip them (server default).
//
// Calling this method makes the choice explicit on the wire, so passing 0 is
// honored instead of falling back to the server default.
func (r *SpeechRecognizer) SetFilterEmptyResult(mode int) {
	r.filterEmptyResult = &mode
}

// SetWordInfo sets whether to show word-level timing information.
// 0: no (default), 1: yes, 2: include punctuation timing.
//
// Word-level speaker attribution (WordInfo.SpeakerID) requires a non-zero
// value together with SetSpeakerDiarization.
func (r *SpeechRecognizer) SetWordInfo(mode int) {
	r.wordInfo = mode
}

// SetVadSilenceTime sets the silence detection threshold in milliseconds.
// Range: 240-2000, default: server-side (currently 800)
func (r *SpeechRecognizer) SetVadSilenceTime(ms int) {
	r.vadSilenceTime = ms
}

// SetVadLevel selects the VAD profile: 0 = high recall, 1 = far-field noise
// filtering (server default).
//
// Calling this method makes the choice explicit on the wire, so passing 0 is
// honored instead of falling back to the server default.
func (r *SpeechRecognizer) SetVadLevel(level int) {
	r.vadLevel = &level
}

// SetNoiseThreshold fine-tunes VAD noise suppression. Valid range: [0, 4];
// larger values suppress more noise at the cost of recall. When set, it
// overrides the profile selected by SetVadLevel.
//
// The value is only sent when this method is called, because 0 is a valid,
// meaningful threshold and cannot be distinguished from "unset" otherwise.
func (r *SpeechRecognizer) SetNoiseThreshold(threshold float64) {
	r.noiseThreshold = &threshold
}

// SetMaxSpeakTime sets the maximum speech time in milliseconds.
// Range: 5000-90000, default: 60000
func (r *SpeechRecognizer) SetMaxSpeakTime(ms int) {
	r.maxSpeakTime = ms
}

// SetInputSampleRate declares the sample rate of the incoming PCM audio.
// Only 8000 is supported, which lets an 8kHz stream be fed to a 16k engine
// (the server upsamples it).
func (r *SpeechRecognizer) SetInputSampleRate(rate int) {
	r.inputSampleRate = rate
}

// SetSpeakerDiarization enables real-time speaker diarization.
//
//	SpeakerDiarizationOff        (0) disabled (default)
//	SpeakerDiarizationCluster    (1) anonymous clustering; speakers are numbered
//	                                 from 1 within the session, -1 = unknown
//	SpeakerDiarizationVoiceprint (3) voiceprint role authentication; combine with
//	                                 SetSpeakerRoles / SetVoiceprintIDs to get
//	                                 role names back in SpeakerName
//
// Results are reported through Result.SpeakerSegments, and additionally
// through WordInfo.SpeakerID when SetWordInfo is non-zero.
func (r *SpeechRecognizer) SetSpeakerDiarization(mode int) {
	r.speakerDiarization = mode
}

// SetSpeakerNumber hints the expected number of speakers. 0 means auto
// detection (default). It applies to both diarization modes: the server feeds
// it into the online clustering.
func (r *SpeechRecognizer) SetSpeakerNumber(n int) {
	r.speakerNumber = n
}

// SetSpeakerRoles registers temporary voiceprints for this session. Each role
// carries a name and the URL of its enrollment audio; the name is echoed back
// as SpeakerName on matched words and speaker segments.
//
// Only used when speaker diarization is set to SpeakerDiarizationVoiceprint.
// The slice is copied, so later mutations by the caller do not affect the
// session.
func (r *SpeechRecognizer) SetSpeakerRoles(roles []SpeakerRole) {
	r.speakerRoles = append([]SpeakerRole(nil), roles...)
}

// SetVoiceprintIDs registers previously enrolled voiceprints by ID for this
// session. Only used when speaker diarization is set to
// SpeakerDiarizationVoiceprint. The slice is copied.
func (r *SpeechRecognizer) SetVoiceprintIDs(ids []string) {
	r.voiceprintIDs = append([]string(nil), ids...)
}

// SetVoiceID sets a custom voice ID. If not set, a UUID will be generated.
func (r *SpeechRecognizer) SetVoiceID(id string) {
	r.voiceID = id
}

// SetLanguage sets the language hint for the bigmodel engine (e.g. "ms", "zh", "auto").
// It is transparently forwarded to the server as the "language" query parameter.
func (r *SpeechRecognizer) SetLanguage(lang string) {
	r.language = lang
}

// SetEndpoint overrides the WebSocket origin (for testing against a mock
// server). A non-empty value wins over Credential.Site.
func (r *SpeechRecognizer) SetEndpoint(endpoint string) {
	r.endpoint = endpoint
}

// SetWriteTimeout sets the timeout for a single audio write.
//
// The value is clamped to [minWriteTimeout, maxWriteTimeout]; a non-positive
// value resets it to the default. Because Stop must acquire the writer to send
// the end signal, an unbounded write timeout would let an in-flight Write delay
// Stop indefinitely — clamping keeps Stop's worst-case exit time predictable
// (roughly writeTimeout to acquire the writer, then up to stopTimeout for the
// server's final response).
func (r *SpeechRecognizer) SetWriteTimeout(timeout time.Duration) {
	switch {
	case timeout <= 0:
		timeout = defaultWriteTimeout
	case timeout < minWriteTimeout:
		timeout = minWriteTimeout
	case timeout > maxWriteTimeout:
		timeout = maxWriteTimeout
	}
	r.writeTimeout = timeout
}

// SetStopTimeout sets how long Stop waits for the server's final response after
// sending the end signal before forcing the connection closed.
//
// The value is clamped to [minStopTimeout, maxStopTimeout]; a non-positive
// value resets it to the default (defaultStopTimeout).
func (r *SpeechRecognizer) SetStopTimeout(timeout time.Duration) {
	switch {
	case timeout <= 0:
		timeout = defaultStopTimeout
	case timeout < minStopTimeout:
		timeout = minStopTimeout
	case timeout > maxStopTimeout:
		timeout = maxStopTimeout
	}
	r.stopTimeout = timeout
}

// Start initiates the WebSocket connection and begins the recognition session.
// It returns an error if the configuration is invalid, the connection fails, or
// the recognizer is already running.
func (r *SpeechRecognizer) Start() error {
	if !atomic.CompareAndSwapInt32(&r.state, stateIdle, stateStarting) {
		return common.NewASRError(common.ErrCodeAlreadyStarted, "recognizer already started")
	}

	// Validate before dialing so an invalid option fails locally instead of
	// costing a connection and coming back as a server-side 4001.
	if err := r.validateOptions(); err != nil {
		atomic.StoreInt32(&r.state, stateIdle)
		return err
	}

	if err := r.connect(); err != nil {
		atomic.StoreInt32(&r.state, stateIdle)
		return err
	}

	atomic.StoreInt32(&r.state, stateRunning)

	// Start reading responses in background
	go r.readLoop()

	return nil
}

// validateOptions checks the options that have a documented server-side range.
func (r *SpeechRecognizer) validateOptions() error {
	if err := validateSpeakerDiarization(r.speakerDiarization, r.speakerNumber, r.speakerRoles, r.voiceprintIDs); err != nil {
		return err
	}
	if err := validateVadTuning(r.vadLevel, r.noiseThreshold); err != nil {
		return err
	}
	if r.filterEmptyResult != nil {
		if err := validateEnumOption("FilterEmptyResult", *r.filterEmptyResult, 0, 1); err != nil {
			return err
		}
	}
	// 8000 is the only supported override; 0 means "use the engine rate".
	return validateEnumOption("InputSampleRate", r.inputSampleRate, 0, 8000)
}

// Write sends audio data to the ASR service for recognition.
// The data should be in the format specified by SetVoiceFormat (default: PCM).
func (r *SpeechRecognizer) Write(data []byte) error {
	if atomic.LoadInt32(&r.state) != stateRunning {
		return common.NewASRError(common.ErrCodeNotStarted, "recognizer not running")
	}

	// Grab the connection under mu (short critical section), then release mu
	// before the potentially blocking network write. This lets close() acquire
	// mu and tear the connection down even while this write is in flight; the
	// in-flight WriteMessage then returns an error instead of blocking Stop.
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()

	if conn == nil {
		return common.NewASRError(common.ErrCodeNotStarted, "connection not established")
	}

	// gorilla/websocket allows only one concurrent writer per connection.
	r.writeMu.Lock()
	defer r.writeMu.Unlock()

	// Re-check the state under writeMu. Between the entry check above and
	// acquiring writeMu, Stop may have transitioned the state and sent the
	// end signal. Writing audio after end would violate the protocol, so bail
	// out instead. (Stop sets the state before taking writeMu to send end, so
	// once we hold writeMu a non-running state means end was sent or is next.)
	if atomic.LoadInt32(&r.state) != stateRunning {
		return common.NewASRError(common.ErrCodeNotStarted, "recognizer not running")
	}

	// SetWriteDeadline only fails on a closed connection; WriteMessage below
	// then surfaces that as a write error, so the deadline error is redundant.
	_ = conn.SetWriteDeadline(time.Now().Add(r.writeTimeout))
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return common.NewASRErrorf(common.ErrCodeWriteFailed, "write audio data failed: %v", err)
	}

	return nil
}

// Stop gracefully stops the recognition session.
//
// It sends the end signal and waits for the server's final response (up to
// stopTimeout) before forcing the connection closed. Worst-case duration is
// bounded by writeTimeout (to acquire the writer) plus stopTimeout.
//
// Stop is safe to call from a recognition callback.
//
// For terminal callbacks (OnRecognitionComplete / terminal OnFail), the
// recognizer has already advanced to stopped before callback dispatch, so Stop
// returns immediately with not-running.
//
// For non-terminal callbacks (for example OnRecognitionResultChange), Stop
// sends the end signal and returns without waiting on doneCh; waiting there
// would self-block because callbacks run on the readLoop goroutine.
func (r *SpeechRecognizer) Stop() error {
	if !atomic.CompareAndSwapInt32(&r.state, stateRunning, stateStopping) {
		return common.NewASRError(common.ErrCodeNotStarted, "recognizer not running")
	}

	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()

	if conn == nil {
		atomic.StoreInt32(&r.state, stateStopped)
		return common.NewASRError(common.ErrCodeNotStarted, "connection not established")
	}

	// Send end signal: serialized with Write via writeMu (not mu), so the
	// timeout-driven close() below can still acquire mu and force the
	// connection closed even if this write blocks.
	endMsg := map[string]string{"type": "end"}
	data, _ := json.Marshal(endMsg)
	r.writeMu.Lock()
	if atomic.LoadInt32(&r.state) == stateStopped {
		r.writeMu.Unlock()
		r.waitForReadLoopOrClose()
		return nil
	}
	_ = conn.SetWriteDeadline(time.Now().Add(r.writeTimeout))
	err := conn.WriteMessage(websocket.TextMessage, data)
	r.writeMu.Unlock()

	if err != nil {
		if atomic.LoadInt32(&r.state) == stateStopped {
			r.waitForReadLoopOrClose()
			return nil
		}
		r.close()
		atomic.StoreInt32(&r.state, stateStopped)
		return common.NewASRErrorf(common.ErrCodeWriteFailed, "send end signal failed: %v", err)
	}

	// If Stop is called from within a listener callback (which is invoked on the
	// readLoop goroutine), waiting on doneCh here would self-block until timeout.
	// In that case, return after sending end; readLoop will continue and finish.
	// The watchdog preserves Stop's timeout semantics if the server never sends a
	// terminal response after receiving end.
	if calledFromListenerCallback() {
		go r.waitForReadLoopOrClose()
		return nil
	}

	// Wait for readLoop to finish with timeout
	r.waitForReadLoopOrClose()

	atomic.StoreInt32(&r.state, stateStopped)
	return nil
}

func (r *SpeechRecognizer) connect() error {
	voiceID := r.voiceID
	if voiceID == "" {
		voiceID = uuid.New().String()
		r.voiceID = voiceID
	}

	// Resolve UserSig locally without mutating the shared credential. Writing
	// back to r.credential.UserSig would race when a single *common.Credential
	// is shared by multiple recognizers started concurrently. This mirrors how
	// Sentence/FileRecognizer.doRequest resolve the signature.
	userSig := r.credential.UserSig
	if userSig == "" {
		var err error
		userSig, err = common.GenUserSig(r.credential.SdkAppID, r.credential.SecretKey, voiceID, 86400)
		if err != nil {
			return common.NewASRErrorf(common.ErrCodeAuthFailed, "generate user sig failed: %v", err)
		}
	}

	// Build request parameters (AppID is used for URL secretid parameter).
	// Authentication identity (sdkappid + usersig) travels in the query string
	// instead of headers, so browser WebSocket clients work without header
	// support; the gateway reads these query parameters when the corresponding
	// headers are absent.
	sigParams := common.NewSignatureParams(r.credential.AppID, r.engineModelType, voiceID)
	sigParams.SdkAppID = r.credential.SdkAppID
	sigParams.VoiceFormat = r.voiceFormat
	sigParams.NeedVad = r.needVad
	sigParams.ConvertNumMode = r.convertNumMode
	sigParams.HotwordID = r.hotwordID
	sigParams.HotwordList = r.hotwordList
	sigParams.CustomizationID = r.customizationID
	sigParams.ReplaceTextID = r.replaceTextID
	sigParams.FilterDirty = r.filterDirty
	sigParams.FilterModal = r.filterModal
	sigParams.FilterPunc = r.filterPunc
	sigParams.FilterEmptyResult = r.filterEmptyResult
	sigParams.WordInfo = r.wordInfo
	sigParams.VadSilenceTime = r.vadSilenceTime
	sigParams.VadLevel = r.vadLevel
	sigParams.NoiseThreshold = r.noiseThreshold
	sigParams.MaxSpeakTime = r.maxSpeakTime
	sigParams.InputSampleRate = r.inputSampleRate
	sigParams.SpeakerDiarization = r.speakerDiarization
	sigParams.SpeakerNumber = r.speakerNumber
	sigParams.SpeakerRoles = r.speakerRoles
	sigParams.VoiceprintIDs = r.voiceprintIDs
	sigParams.Language = r.language

	// Per protocol: signature = UserSig. BuildQueryStringWithSignature also
	// emits usersig so the gateway authenticates without headers.
	queryString := sigParams.BuildQueryStringWithSignature(userSig)
	// URL path uses Tencent Cloud AppID (not SdkAppID)
	base, err := common.ResolveWSEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return err
	}
	wsURL := fmt.Sprintf("%s/asr/v2/%d?%s", base, r.credential.AppID, queryString)

	// No custom headers: the handshake relies on the query string only, which
	// also keeps native browser WebSocket usable.
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return common.NewASRErrorf(common.ErrCodeConnectFailed, "websocket dial failed: %v", err)
	}

	r.conn = conn
	return nil
}

func (r *SpeechRecognizer) readLoop() {
	defer func() {
		// readLoop runs on an SDK-owned goroutine. A panic here — whether from
		// a user-supplied listener callback or from SDK internals — cannot be
		// recovered by the caller (recover only works within the same
		// goroutine), so without this guard it would crash the entire host
		// process that embeds this SDK. Recover, shut down cleanly first, then
		// surface it via OnFail (with the stack to aid diagnosis), so re-entrant
		// Stop/Write calls from OnFail observe the stopped state.
		if rec := recover(); rec != nil {
			err := common.NewASRErrorf(common.ErrCodeReadFailed,
				"recovered from panic in readLoop: %v\n%s", rec, debug.Stack())
			r.finish()
			r.safeOnFail(nil, err)
			r.closeDone()
			return
		}
		// finish() is idempotent (sync.Once); this is the catch-all for exit
		// paths that did not finish explicitly (e.g. a caller-initiated close).
		r.finish()
		r.closeDone()
	}()

	// Capture the connection once. close() may set r.conn = nil concurrently
	// (e.g. when Stop() times out), so reading r.conn on every iteration would
	// race and can dereference a nil pointer. Holding a local reference keeps
	// this loop safe: close() calls conn.Close(), which unblocks ReadMessage
	// with an error and lets the loop exit cleanly.
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()

	if conn == nil {
		return
	}

	r.withListenerCallback(func() {
		r.listener.OnRecognitionStart(&SpeechRecognitionResponse{
			Code:    0,
			Message: "success",
			VoiceID: r.voiceID,
		})
	})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if atomic.LoadInt32(&r.state) >= stateStopping {
				return
			}
			// Terminal: finish the lifecycle before notifying, so a Stop/Write
			// call from inside OnFail sees the stopped state and returns
			// immediately instead of waiting on doneCh (which only this
			// goroutine closes).
			r.finish()
			r.safeOnFail(nil, common.NewASRErrorf(common.ErrCodeReadFailed, "read message failed: %v", err))
			return
		}

		var resp SpeechRecognitionResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			// Non-terminal: the session continues, so do not finish here.
			r.safeOnFail(nil, common.NewASRErrorf(common.ErrCodeReadFailed, "unmarshal response failed: %v", err))
			continue
		}

		if resp.Code != 0 {
			r.finish()
			r.markTerminalResponseReceived()
			r.safeOnFail(&resp, common.NewASRError(resp.Code, resp.Message))
			return
		}

		// Check if recognition is complete before dispatching the terminal
		// response. A Final=1 response can still carry slice_type=2, which
		// dispatches OnSentenceEnd; finish first so Stop/Write from that callback
		// observes the stopped state instead of waiting on doneCh.
		if resp.Final == 1 {
			r.finish()
			r.markTerminalResponseReceived()
			r.dispatchEvent(&resp)
			r.safeComplete(&resp)
			return
		}

		// Skip the connection acknowledgement frame. After connect, the server
		// sends an ack that carries no "result" object
		// (e.g. {"code":0,"message":"success","voice_id":"v1"}). Decoding such a
		// frame into SpeechRecognitionResponse yields a zero-valued Result whose
		// SliceType=0 would otherwise be misread by dispatchEvent as a
		// slice_type=0 "sentence begin", emitting a spurious OnSentenceBegin.
		// The value-typed Result cannot tell "absent" from "zero", so probe the
		// raw payload for the result field and skip frames that lack it. The
		// session start is already signaled via OnRecognitionStart at readLoop
		// entry.
		var probe struct {
			Result *json.RawMessage `json:"result"`
		}
		if json.Unmarshal(message, &probe) != nil || probe.Result == nil {
			continue
		}

		r.dispatchEvent(&resp)
	}
}

func (r *SpeechRecognizer) dispatchEvent(resp *SpeechRecognitionResponse) {
	if resp.Final == 1 && resp.Result.SliceType != 2 {
		return
	}

	switch resp.Result.SliceType {
	case 0:
		r.withListenerCallback(func() { r.listener.OnSentenceBegin(resp) })
	case 1:
		r.withListenerCallback(func() { r.listener.OnRecognitionResultChange(resp) })
	case 2:
		r.withListenerCallback(func() { r.listener.OnSentenceEnd(resp) })
	}
}

func (r *SpeechRecognizer) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conn != nil {
		r.conn.Close()
		r.conn = nil
	}
}

// finish advances the recognizer to the terminal stopped state exactly once and
// closes the connection. It is invoked before terminal callbacks (so a
// Stop/Write from inside a callback returns immediately) and again from
// readLoop's defer as a catch-all. doneCh is closed separately after readLoop
// has delivered any terminal callback, so external Stop callers do not return
// while final callbacks are still running.
func (r *SpeechRecognizer) finish() {
	r.finishOnce.Do(func() {
		atomic.StoreInt32(&r.state, stateStopped)
		r.close()
	})
}

func (r *SpeechRecognizer) closeDone() {
	r.doneOnce.Do(func() {
		close(r.doneCh)
	})
}

func (r *SpeechRecognizer) markTerminalResponseReceived() {
	r.termOnce.Do(func() {
		close(r.terminalCh)
	})
}

func (r *SpeechRecognizer) waitForReadLoopOrClose() {
	timer := time.NewTimer(r.stopTimeout)
	defer timer.Stop()

	select {
	case <-r.doneCh:
	case <-r.terminalCh:
		<-r.doneCh
	case <-timer.C:
		r.close()
	}
}

// safeOnFail delivers an OnFail callback while shielding the SDK's internal
// goroutine from a panic inside the user-supplied listener. A faulty callback
// must never crash the host process, and it must not prevent readLoop's
// deferred cleanup from running.
func (r *SpeechRecognizer) safeOnFail(resp *SpeechRecognitionResponse, err error) {
	defer func() { _ = recover() }()
	r.withListenerCallback(func() { r.listener.OnFail(resp, err) })
}

// safeComplete delivers the OnRecognitionComplete callback with the same
// panic-shielding guarantee as safeOnFail.
func (r *SpeechRecognizer) safeComplete(resp *SpeechRecognitionResponse) {
	defer func() { _ = recover() }()
	r.withListenerCallback(func() { r.listener.OnRecognitionComplete(resp) })
}

//go:noinline
func (r *SpeechRecognizer) withListenerCallback(fn func()) {
	fn()
}

// calledFromListenerCallback reports whether the current call stack passes
// through withListenerCallback, i.e. Stop is being re-entered from within a
// listener callback on the readLoop goroutine. It walks the stack (rather than
// using a flag) so that an external goroutine calling Stop while readLoop is
// inside a callback is NOT mistaken for re-entry — that caller must still wait
// for the terminal response.
//
// Known limitation: detection is capped at 256 stack frames. If a callback
// reaches Stop through a deeper call chain, this returns false and the
// re-entrant Stop degrades to external-Stop semantics (it may self-wait up to
// stopTimeout). Such depths are not expected in practice.
func calledFromListenerCallback() bool {
	for depth := 32; depth <= 256; depth *= 2 {
		pcs := make([]uintptr, depth)
		n := runtime.Callers(2, pcs)
		frames := runtime.CallersFrames(pcs[:n])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, ".(*SpeechRecognizer).withListenerCallback") {
				return true
			}
			if !more {
				break
			}
		}
		if n < depth {
			return false
		}
	}
	return false
}
