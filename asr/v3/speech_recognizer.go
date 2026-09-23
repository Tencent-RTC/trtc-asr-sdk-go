// speech_recognizer.go implements the v3 real-time speech recognition client
// (WebSocket /asr/v3).
//
// Protocol recap:
//
//  1. Dial wss://{host}/asr/v3?voice_id=<id> — the URL carries only voice_id.
//  2. Within 3s of the handshake, send one text frame:
//     {"type":"start","auth":{...},"params":{...}} (≤64KB).
//  3. The server acks {"code":0,...} or fails with a structured error frame
//     followed by a normal close.
//  4. Stream audio as binary frames (≤256KB each), then send {"type":"end"}.
//  5. Downlink result frames are identical to v2: {code, message, voice_id,
//     message_id, result{slice_type,...}, final}.
//
// Usage:
//
//	credential := v3.NewCredential(sdkAppID, secretKey)
//	listener := &MyListener{}
//	recognizer := v3.NewSpeechRecognizer(credential, "16k_zh_en", listener)
//	recognizer.Start()
//	recognizer.Write(audioData)
//	recognizer.Stop()
package v3

import (
	"encoding/json"
	"fmt"
	"net/url"
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

const (
	// Write-timeout bounds. A single Write holds the writer for at most
	// writeTimeout (enforced via SetWriteDeadline), so Stop's worst-case wait
	// to acquire the writer for the end signal is bounded by writeTimeout.
	defaultWriteTimeout = 5 * time.Second
	minWriteTimeout     = 50 * time.Millisecond
	maxWriteTimeout     = 30 * time.Second

	// Stop-timeout bounds. stopTimeout caps how long Stop waits for the
	// server's final response after the end signal before forcing the
	// connection closed.
	defaultStopTimeout = 10 * time.Second
	minStopTimeout     = 1 * time.Second
	maxStopTimeout     = 60 * time.Second

	// ackTimeout caps how long Start waits for the server's start-frame
	// acknowledgement. The server answers right after authentication and
	// parameter checks, so this only fires on a genuinely unhealthy link.
	ackTimeout = 5 * time.Second

	// speakerContextAckTimeout replaces ackTimeout while resuming a speaker
	// context (sync mode + a stored speaker_context_id): the first response
	// is delayed until the server has loaded the stored speaker snapshot
	// from COS and restored it in the diarization session, which is slower
	// than the plain handshake but still bounded.
	speakerContextAckTimeout = 15 * time.Second
)

// Wire size limits, mirroring the server side. Checked locally so an
// oversized frame fails before touching the network.
const (
	startFrameMaxBytes  = 64 * 1024  // start frame
	streamFrameMaxBytes = 256 * 1024 // single audio frame
)

// SpeechRecognitionListener defines the callback interface for recognition
// events. Callers that only care about a subset can embed
// UnimplementedSpeechRecognitionListener and override what they need.
type SpeechRecognitionListener interface {
	// OnRecognitionStart is called when the session starts successfully
	// (the server's start-frame ack has been received).
	OnRecognitionStart(response *SpeechRecognitionResponse)
	// OnSentenceBegin is called when a new sentence begins.
	OnSentenceBegin(response *SpeechRecognitionResponse)
	// OnRecognitionResultChange is called on intermediate results.
	OnRecognitionResultChange(response *SpeechRecognitionResponse)
	// OnSentenceEnd is called when a sentence ends with the final result.
	OnSentenceEnd(response *SpeechRecognitionResponse)
	// OnRecognitionComplete is called when the whole session completes.
	OnRecognitionComplete(response *SpeechRecognitionResponse)
	// OnFail is called when an error occurs during recognition.
	OnFail(response *SpeechRecognitionResponse, err error)
}

// UnimplementedSpeechRecognitionListener is a no-op SpeechRecognitionListener.
// Embed it and override only the events you care about.
type UnimplementedSpeechRecognitionListener struct{}

func (UnimplementedSpeechRecognitionListener) OnRecognitionStart(*SpeechRecognitionResponse)        {}
func (UnimplementedSpeechRecognitionListener) OnSentenceBegin(*SpeechRecognitionResponse)           {}
func (UnimplementedSpeechRecognitionListener) OnRecognitionResultChange(*SpeechRecognitionResponse) {}
func (UnimplementedSpeechRecognitionListener) OnSentenceEnd(*SpeechRecognitionResponse)             {}
func (UnimplementedSpeechRecognitionListener) OnRecognitionComplete(*SpeechRecognitionResponse)     {}
func (UnimplementedSpeechRecognitionListener) OnFail(*SpeechRecognitionResponse, error)             {}

var _ SpeechRecognitionListener = UnimplementedSpeechRecognitionListener{}
var _ SpeechRecognitionListener = (*UnimplementedSpeechRecognitionListener)(nil)

// SpeechRecognitionResponse is a downlink message from the ASR service. The
// v3 downlink shape is identical to v2.
type SpeechRecognitionResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	VoiceID   string `json:"voice_id"`
	MessageID string `json:"message_id"`
	Final     int    `json:"final"`
	Result    Result `json:"result"`

	// SpeakerContinue is the speaker-context result carried by the first
	// response of a session that enabled the speaker context (see
	// SetEnableSpeakerContext); it also appears on the OnRecognitionStart
	// callback. Nil when the session did not enable it.
	SpeakerContinue *SpeakerContinue `json:"speaker_continue,omitempty"`
}

// Result contains the recognition result details.
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

// onlineParams is the v3 start frame params block (snake_case wire). Pointer
// fields distinguish "not sent" from an explicit zero.
type onlineParams struct {
	VoiceID            string   `json:"voice_id,omitempty"`
	EngineModelType    string   `json:"engine_model_type"`
	Language           string   `json:"language,omitempty"`
	VoiceFormat        int      `json:"voice_format"`
	InputSampleRate    *int     `json:"input_sample_rate,omitempty"`
	Needvad            *int     `json:"needvad,omitempty"`
	VadSilenceTimeMs   *int     `json:"vad_silence_time,omitempty"`
	VadLevel           *int     `json:"vad_level,omitempty"`
	NoiseThreshold     *float64 `json:"noise_threshold,omitempty"`
	MaxSpeakTime       int      `json:"max_speak_time,omitempty"`
	FilterDirty        int      `json:"filter_dirty,omitempty"`
	FilterModal        int      `json:"filter_modal,omitempty"`
	FilterPunc         int      `json:"filter_punc,omitempty"`
	FilterEmptyResult  *int     `json:"filter_empty_result,omitempty"`
	ConvertNumMode     *int     `json:"convert_num_mode,omitempty"`
	WordInfo           int      `json:"word_info,omitempty"`
	WordWithSpace      int      `json:"word_with_space,omitempty"`
	HotwordID          string   `json:"hotword_id,omitempty"`
	HotwordList        string   `json:"hotword_list,omitempty"`
	SpeakerDiarization int      `json:"speaker_diarization,omitempty"`
	SpeakerNumber      int      `json:"speaker_number,omitempty"`
	// EnableSpeakerContext: 1 (sync) / 2 (async) make the diarization session
	// resumable; 0 is omitted. SpeakerContextID is the opaque id issued by a
	// previous session and is only meaningful together with the mode.
	EnableSpeakerContext int           `json:"enable_speaker_context,omitempty"`
	SpeakerContextID     string        `json:"speaker_context_id,omitempty"`
	VoiceprintIDs        []string      `json:"voiceprint_ids,omitempty"`
	SpeakerRoles         []SpeakerRole `json:"speaker_roles,omitempty"`
	Context              *Context      `json:"context,omitempty"`

	// SDKInfo carries the SDK self-identification (platform / sdk_lang /
	// sdk_type / version). It is not a protocol field: the gateway replays the
	// start frame byte-for-byte to the worker, which ignores unknown keys, so
	// the telemetry survives in server-side dumps without disturbing the
	// protocol.
	SDKInfo map[string]string `json:"sdk_info,omitempty"`
}

// startFrame is the v3 first frame: {"type":"start","auth":{...},"params":{...}}.
type startFrame struct {
	Type   string        `json:"type"`
	Auth   authBlock     `json:"auth"`
	Params *onlineParams `json:"params"`
}

// SpeechRecognizer is the v3 real-time speech recognition client.
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
	filterDirty        int
	filterModal        int
	filterPunc         int
	filterEmptyResult  *int
	wordInfo           int
	wordWithSpace      int
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
	language           string
	context            *Context

	// Speaker context ("断点续传"): enableSpeakerContext is the requested mode
	// (0/1/2), speakerContextID the id issued by an earlier session.
	enableSpeakerContext int
	speakerContextID     string

	// speakerContinue holds the handshake result of the first response
	// (speaker_continue). Written by connect before Start returns and read by
	// readLoop and by callers, hence the atomic pointer.
	speakerContinue atomic.Pointer[SpeakerContinue]

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
//   - credential: TRTC authentication credential (v3 needs only SdkAppID +
//     SecretKey; see NewCredential)
//   - engineModelType: recognition engine model (e.g., "16k_zh", "8k_zh",
//     "16k_zh_en")
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
// 0: disable, 1: enable (default). Unlike the v2 transport, v3 honors an
// explicit 0 (it is sent on the wire, not dropped).
func (r *SpeechRecognizer) SetNeedVad(needVad int) {
	r.needVad = needVad
}

// SetConvertNumMode sets the number conversion mode.
// 0: no conversion, 1: smart conversion (default), 3: math conversion.
// Unlike the v2 transport, v3 honors an explicit 0.
func (r *SpeechRecognizer) SetConvertNumMode(mode int) {
	r.convertNumMode = mode
}

// SetHotwordID sets the hotword list ID (sdkappid-scoped v3 vocabulary).
func (r *SpeechRecognizer) SetHotwordID(id string) {
	r.hotwordID = id
}

// SetHotwordList sets a temporary inline hotword list, which does not require
// creating a hotword table on the console.
//
// Format: "word1|weight1,word2|weight2". Each word is at most 30 chars and
// the weight must be 1-11 (11 = super hotword) or 100 (homophone replacement).
func (r *SpeechRecognizer) SetHotwordList(list string) {
	r.hotwordList = list
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
func (r *SpeechRecognizer) SetFilterEmptyResult(mode int) {
	r.filterEmptyResult = &mode
}

// SetWordInfo sets whether to show word-level timing information.
// 0: no (default), 1: yes, 2: include punctuation timing, 100: caption mode.
func (r *SpeechRecognizer) SetWordInfo(mode int) {
	r.wordInfo = mode
}

// SetWordWithSpace sets whether English words are joined with spaces.
// 0: no (default), 1: yes.
func (r *SpeechRecognizer) SetWordWithSpace(mode int) {
	r.wordWithSpace = mode
}

// SetVadSilenceTime sets the silence detection threshold in milliseconds.
// Range: 240-2000 (with VAD enabled), default: server-side (currently 800)
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
//	SpeakerDiarizationCluster    (1) anonymous clustering
//	SpeakerDiarizationVoiceprint (3) voiceprint role authentication; combine
//	                                 with SetSpeakerRoles / SetVoiceprintIDs
func (r *SpeechRecognizer) SetSpeakerDiarization(mode int) {
	r.speakerDiarization = mode
}

// SetSpeakerNumber hints the expected number of speakers. 0 means auto
// detection (default).
func (r *SpeechRecognizer) SetSpeakerNumber(n int) {
	r.speakerNumber = n
}

// SetSpeakerRoles registers temporary voiceprints for this session. Only used
// when speaker diarization is set to SpeakerDiarizationVoiceprint. The slice
// is copied.
func (r *SpeechRecognizer) SetSpeakerRoles(roles []SpeakerRole) {
	r.speakerRoles = append([]SpeakerRole(nil), roles...)
}

// SetVoiceprintIDs registers previously enrolled voiceprints by ID for this
// session. Only used with SpeakerDiarizationVoiceprint. The slice is copied.
func (r *SpeechRecognizer) SetVoiceprintIDs(ids []string) {
	r.voiceprintIDs = append([]string(nil), ids...)
}

// SetEnableSpeakerContext makes the speaker-diarization session resumable
// ("说话人分离断点续传") and selects how the server reports the handshake:
//
//	SpeakerContextOff   (0) off (default): no speaker context is saved or
//	                        returned, and SetSpeakerContextID is ignored
//	SpeakerContextSync  (1) the first response waits for the stored speaker
//	                        snapshot to be applied and reports the outcome
//	                        through SpeakerContinue.ContinueStatus
//	SpeakerContextAsync (2) the first response answers immediately with the
//	                        speaker_context_id only (no status); use this to
//	                        keep reconnects fast
//
// Requires SetSpeakerDiarization(1) or (3). See SpeakerContinue for the
// reconnect workflow.
func (r *SpeechRecognizer) SetEnableSpeakerContext(mode int) {
	r.enableSpeakerContext = mode
}

// SetSpeakerContextID passes back the speaker_context_id returned by a
// previous session (SpeakerContinue.SpeakerContextID) so this session resumes
// the same speaker identities instead of numbering speakers from scratch.
//
// It only takes effect together with SetEnableSpeakerContext(1) or (2). The
// server ignores an expired or unknown id and starts a new session, so a stale
// value does not fail the connection; always overwrite the stored id with the
// one returned by the latest first response.
func (r *SpeechRecognizer) SetSpeakerContextID(id string) {
	r.speakerContextID = strings.TrimSpace(id)
}

// SpeakerContinue returns the speaker-context result carried by the first
// server response, or nil when the session did not enable the speaker context
// (or has not been started yet). It is available once Start returns and is
// also delivered to OnRecognitionStart.
func (r *SpeechRecognizer) SpeakerContinue() *SpeakerContinue {
	return r.speakerContinue.Load()
}

// SetVoiceID sets a custom voice ID. If not set, a UUID will be generated.
// The UserSig is bound to this value, so a custom voice ID is signed
// automatically — no extra work needed.
func (r *SpeechRecognizer) SetVoiceID(id string) {
	r.voiceID = id
}

// SetLanguage sets the language hint (e.g. "zh", "en", "auto").
func (r *SpeechRecognizer) SetLanguage(lang string) {
	r.language = lang
}

// SetContext sets the recognition context (background text, domain terms,
// domain key-values). LLM-class engines consume all parts; traditional
// engines degrade Context.Terms to hotwords.
func (r *SpeechRecognizer) SetContext(ctx *Context) {
	r.context = ctx
}

// SetEndpoint overrides the WebSocket origin (for testing against a mock
// server). A non-empty value wins over Credential.Site.
func (r *SpeechRecognizer) SetEndpoint(endpoint string) {
	r.endpoint = endpoint
}

// SetWriteTimeout sets the timeout for a single audio write, clamped to
// [minWriteTimeout, maxWriteTimeout]; a non-positive value resets the default.
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

// SetStopTimeout sets how long Stop waits for the server's final response
// after sending the end signal before forcing the connection closed, clamped
// to [minStopTimeout, maxStopTimeout].
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

// Start connects, sends the start frame and waits (up to ackTimeout) for the
// server acknowledgement. Authentication and parameter errors (4001/4002/...)
// are therefore returned synchronously instead of surfacing later via OnFail.
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

	firstResult, err := r.connect()
	if err != nil {
		atomic.StoreInt32(&r.state, stateIdle)
		return err
	}

	atomic.StoreInt32(&r.state, stateRunning)

	// Start reading responses in background
	go r.readLoop(firstResult)

	return nil
}

// Server-side accepted ranges (asr-proxy validator_v3.go).
const (
	maxVoiceIDLen       = 128
	minMaxSpeakTime     = 5000
	maxMaxSpeakTime     = 90000
	minVadSilenceTimeMs = 240
	maxVadSilenceTimeMs = 2000
)

// validVoiceFormats mirrors the server-side voice_format whitelist.
var validVoiceFormats = []int{1, 4, 6, 8, 10, 11, 12, 14, 16}

// validateOptions checks the options that have a documented server-side
// range, mirroring the proxy's v3 online validator so an invalid value fails
// locally instead of coming back as a remote 4001.
func (r *SpeechRecognizer) validateOptions() error {
	if len(r.voiceID) > maxVoiceIDLen {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"VoiceID length must not exceed %d", maxVoiceIDLen)
	}
	if err := validateSpeakerDiarization(r.speakerDiarization, r.speakerNumber, r.speakerRoles, r.voiceprintIDs); err != nil {
		return err
	}
	if err := validateSpeakerContext(r.enableSpeakerContext, r.speakerDiarization); err != nil {
		return err
	}
	if err := validateVadTuning(r.vadLevel, r.noiseThreshold); err != nil {
		return err
	}
	if r.maxSpeakTime != 0 && (r.maxSpeakTime < minMaxSpeakTime || r.maxSpeakTime > maxMaxSpeakTime) {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"MaxSpeakTime must be between %d and %d ms, got %d",
			minMaxSpeakTime, maxMaxSpeakTime, r.maxSpeakTime)
	}
	// vadSilenceTime==0 means "not set" (the field is then omitted on the
	// wire); the range check only applies to an explicitly set value with VAD
	// enabled, matching the server rule.
	if r.vadSilenceTime != 0 && r.needVad == 1 &&
		(r.vadSilenceTime < minVadSilenceTimeMs || r.vadSilenceTime > maxVadSilenceTimeMs) {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"VadSilenceTime must be between %d and %d ms (needvad=1), got %d",
			minVadSilenceTimeMs, maxVadSilenceTimeMs, r.vadSilenceTime)
	}
	checks := []struct {
		name    string
		value   int
		allowed []int
	}{
		{"NeedVad", r.needVad, []int{0, 1}},
		{"ConvertNumMode", r.convertNumMode, []int{0, 1, 3}},
		{"FilterDirty", r.filterDirty, []int{0, 1, 2}},
		{"FilterModal", r.filterModal, []int{0, 1, 2}},
		{"FilterPunc", r.filterPunc, []int{0, 1}},
		{"WordInfo", r.wordInfo, []int{0, 1, 2, 100}},
		{"WordWithSpace", r.wordWithSpace, []int{0, 1}},
		{"VoiceFormat", r.voiceFormat, validVoiceFormats},
		// 8000 is the only supported override; 0 means "use the engine rate".
		{"InputSampleRate", r.inputSampleRate, []int{0, 8000}},
	}
	for _, c := range checks {
		if err := validateEnumOption(c.name, c.value, c.allowed...); err != nil {
			return err
		}
	}
	if r.filterEmptyResult != nil {
		if err := validateEnumOption("FilterEmptyResult", *r.filterEmptyResult, 0, 1); err != nil {
			return err
		}
	}
	return nil
}

// Write sends one audio frame (binary) to the ASR service.
func (r *SpeechRecognizer) Write(data []byte) error {
	if atomic.LoadInt32(&r.state) != stateRunning {
		return common.NewASRError(common.ErrCodeNotStarted, "recognizer not running")
	}
	if len(data) > streamFrameMaxBytes {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"audio frame exceeds %d bytes", streamFrameMaxBytes)
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
	// out instead.
	if atomic.LoadInt32(&r.state) != stateRunning {
		return common.NewASRError(common.ErrCodeNotStarted, "recognizer not running")
	}

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

	// If Stop is called from within a listener callback (which is invoked on
	// the readLoop goroutine), waiting on doneCh here would self-block until
	// timeout. In that case, return after sending end; readLoop will continue
	// and finish. The watchdog preserves Stop's timeout semantics if the
	// server never sends a terminal response after receiving end.
	if calledFromListenerCallback() {
		go r.waitForReadLoopOrClose()
		return nil
	}

	// Wait for readLoop to finish with timeout
	r.waitForReadLoopOrClose()

	atomic.StoreInt32(&r.state, stateStopped)
	return nil
}

// ackWait returns how long connect waits for the first response. Resuming a
// speaker context in sync mode makes the server apply the stored snapshot
// before answering the handshake, so that path gets a longer budget than the
// plain handshake; every other case answers immediately.
func (r *SpeechRecognizer) ackWait() time.Duration {
	if r.enableSpeakerContext == SpeakerContextSync && r.speakerContextID != "" {
		return speakerContextAckTimeout
	}
	return ackTimeout
}

// speakerContextIDForWire returns the id only when the caller opted in.
// Mode 0 omits enable_speaker_context via omitempty, but a non-empty id
// would still be serialized.
func (r *SpeechRecognizer) speakerContextIDForWire() string {
	if r.enableSpeakerContext == SpeakerContextOff {
		return ""
	}
	return r.speakerContextID
}

// connect dials /asr/v3, sends the start frame and waits for the server ack.
// It returns the first downlink frame when that frame already carries a
// result (the ack frame itself is consumed here and not reported again).
func (r *SpeechRecognizer) connect() ([]byte, error) {
	voiceID := r.voiceID
	if voiceID == "" {
		voiceID = uuid.New().String()
		r.voiceID = voiceID
	}

	// Resolve UserSig locally without mutating the shared credential. The v3
	// signature identifier is the voice_id.
	userSig := r.credential.UserSig
	if userSig == "" {
		var err error
		userSig, err = common.GenUserSig(r.credential.SdkAppID, r.credential.SecretKey, voiceID, 86400)
		if err != nil {
			return nil, common.NewASRErrorf(common.ErrCodeAuthFailed, "generate user sig failed: %v", err)
		}
	}

	frame, err := r.buildStartFrame(userSig)
	if err != nil {
		return nil, err
	}
	if len(frame) > startFrameMaxBytes {
		return nil, common.NewASRErrorf(common.ErrCodeInvalidParam,
			"start frame exceeds %d bytes (%d)", startFrameMaxBytes, len(frame))
	}

	base, err := common.ResolveWSEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	// The URL carries only voice_id; auth and params travel in the start
	// frame, so the handshake stays header-free and browser-friendly.
	wsURL := fmt.Sprintf("%s/asr/v3?voice_id=%s", base, url.QueryEscape(voiceID))

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeConnectFailed, "websocket dial failed: %v", err)
	}

	// Send the start frame immediately (the server enforces a 3s deadline).
	_ = conn.SetWriteDeadline(time.Now().Add(r.writeTimeout))
	if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
		conn.Close()
		return nil, common.NewASRErrorf(common.ErrCodeWriteFailed, "send start frame failed: %v", err)
	}

	// Wait for the ack. A failure arrives as a structured error frame
	// ({code,message,voice_id}) followed by a normal close.
	_ = conn.SetReadDeadline(time.Now().Add(r.ackWait()))
	messageType, message, err := conn.ReadMessage()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return nil, common.NewASRErrorf(common.ErrCodeReadFailed, "read start ack failed: %v", err)
	}
	if messageType != websocket.TextMessage {
		conn.Close()
		return nil, common.NewASRErrorf(common.ErrCodeServerError,
			"unexpected non-text start ack (type %d)", messageType)
	}
	var ack struct {
		Code    int              `json:"code"`
		Message string           `json:"message"`
		Result  *json.RawMessage `json:"result"`
		// speaker_continue is present only when the session enabled the
		// speaker context; it carries the id to persist for a later resume.
		SpeakerContinue *SpeakerContinue `json:"speaker_continue"`
	}
	if err := json.Unmarshal(message, &ack); err != nil {
		conn.Close()
		return nil, common.NewASRErrorf(common.ErrCodeServerError, "invalid start ack: %v", err)
	}
	if ack.Code != 0 {
		conn.Close()
		return nil, serverError(ack.Code, ack.Message, "")
	}
	r.speakerContinue.Store(ack.SpeakerContinue)

	r.conn = conn
	if ack.Result != nil {
		return message, nil
	}
	return nil, nil
}

// buildStartFrame assembles {"type":"start","auth":{...},"params":{...}}.
func (r *SpeechRecognizer) buildStartFrame(userSig string) ([]byte, error) {
	params := &onlineParams{
		VoiceID:            r.voiceID,
		EngineModelType:    r.engineModelType,
		Language:           r.language,
		VoiceFormat:        r.voiceFormat,
		Needvad:            intPtr(r.needVad),
		VadLevel:           r.vadLevel,
		NoiseThreshold:     r.noiseThreshold,
		MaxSpeakTime:       r.maxSpeakTime,
		FilterDirty:        r.filterDirty,
		FilterModal:        r.filterModal,
		FilterPunc:         r.filterPunc,
		FilterEmptyResult:  r.filterEmptyResult,
		ConvertNumMode:     intPtr(r.convertNumMode),
		WordInfo:           r.wordInfo,
		WordWithSpace:      r.wordWithSpace,
		HotwordID:          r.hotwordID,
		HotwordList:        r.hotwordList,
		SpeakerDiarization: r.speakerDiarization,
		SpeakerNumber:      r.speakerNumber,

		// enable_speaker_context / speaker_context_id are only sent when the
		// caller opted in. A stored id with the mode left at 0 must not ride
		// along: omitempty would still emit a non-empty string.
		EnableSpeakerContext: r.enableSpeakerContext,
		SpeakerContextID:     r.speakerContextIDForWire(),
		VoiceprintIDs:        r.voiceprintIDs,
		SpeakerRoles:         r.speakerRoles,
		Context:              r.context,
		SDKInfo:              common.SDKReportParams(),
	}
	if r.inputSampleRate != 0 {
		params.InputSampleRate = intPtr(r.inputSampleRate)
	}
	if r.vadSilenceTime != 0 {
		params.VadSilenceTimeMs = intPtr(r.vadSilenceTime)
	}

	frame := startFrame{
		Type: "start",
		Auth: authBlock{
			SdkAppID: itoa(r.credential.SdkAppID),
			UserSig:  userSig,
		},
		Params: params,
	}
	return json.Marshal(frame)
}

// readLoop drains downlink frames until a terminal response, a read error or
// a caller-initiated close. firstFrame, when non-nil, is a frame that arrived
// before the ack during connect and already carries a result.
func (r *SpeechRecognizer) readLoop(firstFrame []byte) {
	defer func() {
		// readLoop runs on an SDK-owned goroutine. A panic here — whether from
		// a user-supplied listener callback or from SDK internals — cannot be
		// recovered by the caller, so without this guard it would crash the
		// host process. Recover, shut down cleanly first, then surface it via
		// OnFail so re-entrant Stop/Write calls from OnFail see the stopped
		// state.
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
	// race and can dereference a nil pointer.
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
			// The ack itself is consumed by connect; re-attach the speaker
			// context it carried so callback-style callers see the id/status.
			SpeakerContinue: r.speakerContinue.Load(),
		})
	})

	if firstFrame != nil {
		if !r.handleMessage(firstFrame) {
			return
		}
	}

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if atomic.LoadInt32(&r.state) >= stateStopping {
				return
			}
			r.finish()
			r.safeOnFail(nil, common.NewASRErrorf(common.ErrCodeReadFailed, "read message failed: %v", err))
			return
		}
		if !r.handleMessage(message) {
			return
		}
	}
}

// handleMessage dispatches one downlink frame. It returns false when the loop
// must stop (terminal response or error).
func (r *SpeechRecognizer) handleMessage(message []byte) bool {
	var resp SpeechRecognitionResponse
	if err := json.Unmarshal(message, &resp); err != nil {
		// Non-terminal: the session continues, so do not finish here.
		r.safeOnFail(nil, common.NewASRErrorf(common.ErrCodeReadFailed, "unmarshal response failed: %v", err))
		return true
	}

	if resp.Code != 0 {
		r.finish()
		r.markTerminalResponseReceived()
		r.safeOnFail(&resp, common.NewASRError(resp.Code, resp.Message))
		return false
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
		return false
	}

	// Skip frames without a "result" object (defensive: the ack frame is
	// consumed by Start, but a frame lacking result must not be misread as a
	// slice_type=0 sentence begin).
	var probe struct {
		Result *json.RawMessage `json:"result"`
	}
	if json.Unmarshal(message, &probe) != nil || probe.Result == nil {
		return true
	}

	r.dispatchEvent(&resp)
	return true
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

// finish advances the recognizer to the terminal stopped state exactly once
// and closes the connection. It is invoked before terminal callbacks (so a
// Stop/Write from inside a callback returns immediately) and again from
// readLoop's defer as a catch-all.
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
// goroutine from a panic inside the user-supplied listener.
func (r *SpeechRecognizer) safeOnFail(resp *SpeechRecognitionResponse, err error) {
	defer func() { _ = recover() }()
	r.withListenerCallback(func() { r.listener.OnFail(resp, err) })
}

// safeComplete delivers an OnRecognitionComplete callback with the same
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
// listener callback on the readLoop goroutine. It walks the stack (rather
// than using a flag) so that an external goroutine calling Stop while
// readLoop is inside a callback is NOT mistaken for re-entry.
//
// Known limitation: detection is capped at 256 stack frames.
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
