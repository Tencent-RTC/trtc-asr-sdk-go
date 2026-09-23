// types.go holds the v3 wire types shared by the three recognizers.
//
// Request structs marshal directly to the snake_case params block; response
// structs are the flat server shapes (code/message/request_id + payload).
package v3

import (
	"fmt"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// SourceType indicates the audio data source for the HTTP interfaces.
const (
	SourceTypeURL  = 0 // Audio from a URL
	SourceTypeData = 1 // Audio data in request body (base64 encoded)
)

// Task status values returned by describe_transcription.
const (
	TaskStatusWaiting = 0 // Task is queued
	TaskStatusRunning = 1 // Task is being processed
	TaskStatusSuccess = 2 // Task completed successfully
	TaskStatusFailed  = 3 // Task failed
)

// Speaker diarization modes for the speaker_diarization parameter.
const (
	// SpeakerDiarizationOff disables speaker diarization (default).
	SpeakerDiarizationOff = 0
	// SpeakerDiarizationCluster enables anonymous clustering: speakers are
	// numbered from 1 within the current session, -1 means unknown.
	SpeakerDiarizationCluster = 1
	// SpeakerDiarizationVoiceprint enables voiceprint-based role
	// authentication. Combine with SpeakerRoles (temporary enrollment audio)
	// and/or VoiceprintIDs (pre-registered voiceprints) so that recognized
	// speakers carry their role name.
	SpeakerDiarizationVoiceprint = 3
)

// Speaker-context modes for the enable_speaker_context parameter. They turn
// speaker diarization into a resumable ("断点续传") session: the server stores
// the stable speaker anchors of the session and hands back an opaque
// speaker_context_id, which a later connection passes in to keep the same
// speakers on the same IDs. Both modes require speaker diarization.
const (
	// SpeakerContextOff disables speaker-context persistence (default).
	SpeakerContextOff = 0
	// SpeakerContextSync declares the intent to save/resume the speaker
	// context and makes the first server response wait for the stored
	// snapshot, so it reports how the session started through
	// SpeakerContinue.ContinueStatus ("fresh" / "resumed" / "degraded") and
	// carries the authoritative speaker_context_id.
	SpeakerContextSync = 1
	// SpeakerContextAsync declares the same intent but lets the server answer
	// the handshake before the snapshot is loaded: the first response carries
	// the speaker_context_id but never a continue status.
	SpeakerContextAsync = 2
)

// ContinueStatus values of SpeakerContinue.ContinueStatus.
const (
	// ContinueStatusFresh means no stored speaker context was applied: the
	// session starts from scratch and SpeakerContextID is the ID to persist.
	ContinueStatusFresh = "fresh"
	// ContinueStatusResumed means the speaker anchors stored under the
	// requested speaker_context_id were restored; speaker IDs continue from
	// the previous session.
	ContinueStatusResumed = "resumed"
	// ContinueStatusDegraded means a stored context was requested but could
	// not be applied (load or restore failure). The session keeps running as
	// a new one and speaker IDs may restart.
	ContinueStatusDegraded = "degraded"
	// ContinueStatusDisabled means the server did not persist the speaker
	// context for this session.
	ContinueStatusDisabled = "disabled"
)

// SpeakerContinue is the speaker-context handshake result carried by the
// first server response (`speaker_continue`). It is absent unless the session
// set EnableSpeakerContext.
//
// Persist SpeakerContextID on the client side and pass it back through
// SetSpeakerContextID when reconnecting within its lifetime (24h by default);
// the response of a later session is authoritative, so always overwrite the
// stored value. Callers must wait for the first response before sending
// audio: with SpeakerContextSync and a stored ID the server answers only
// after the snapshot has been applied.
type SpeakerContinue struct {
	// ContinueStatus is one of ContinueStatusFresh / ContinueStatusResumed /
	// ContinueStatusDegraded / ContinueStatusDisabled. It is empty in
	// SpeakerContextAsync mode, where the server answers before the snapshot
	// is loaded. Treat unknown values as "no information".
	ContinueStatus string `json:"continue_status,omitempty"`

	// SpeakerContextID is the opaque context id of this speaker session. Pass
	// it back with SetSpeakerContextID to resume the same speaker identities
	// in a new connection; it is not a credential and is scoped to the
	// SdkAppID that issued it.
	SpeakerContextID string `json:"speaker_context_id,omitempty"`
}

// NewCredential creates a credential for the v3 API. v3 uses SdkAppID as the
// only customer dimension and does not need the Tencent Cloud AppID.
func NewCredential(sdkAppID int, secretKey string) *common.Credential {
	return common.NewCredential(0, sdkAppID, secretKey)
}

// ===== auth block =====

// authBlock is the v3 authentication block (start frame / offline body).
// Consumed by the gateway only; params never carry these fields.
type authBlock struct {
	SdkAppID  string `json:"sdkappid"`
	UserSig   string `json:"usersig"`
	RequestID string `json:"request_id,omitempty"` // offline: UserSig identifier
}

// offlineEnvelope is the v3 offline HTTP request wire: {"auth":{}, "params":{}}.
type offlineEnvelope struct {
	Auth   authBlock   `json:"auth"`
	Params interface{} `json:"params"`
}

// ===== shared params types =====

// ContextKV is a domain key-value pair of Context.General
// (e.g. {"key":"domain","value":"Healthcare"}).
type ContextKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Context is the recognition context, aligned with Soniox / Volcengine
// semantics. LLM-class engines can consume all three parts; traditional
// engines degrade Terms to hotwords and ignore Text/General.
type Context struct {
	Text    string      `json:"text,omitempty"`    // background / preceding text
	Terms   []string    `json:"terms,omitempty"`   // domain terms
	General []ContextKV `json:"general,omitempty"` // domain key-values
}

// SpeakerRole is a temporary voiceprint enrollment entry used with
// speaker_diarization=3. RoleName is echoed back as speaker_name /
// speaker_role_name on matched results.
type SpeakerRole struct {
	// RoleName is the caller-defined speaker label (e.g. "teacher").
	RoleName string `json:"role_name"`
	// AudioURL points to the enrollment audio for this role.
	AudioURL string `json:"audio_url"`
}

// AudioURLItem is one audio piece of a distributed recording task.
type AudioURLItem struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

// Word is a word-level timing entry (shared by transcribe and describe
// responses).
type Word struct {
	Word      string `json:"word"`
	StartTime int    `json:"start_time"`
	EndTime   int    `json:"end_time"`
}

// ===== offline responses =====

// TranscribeResponse is the flat /v3/transcribe response.
type TranscribeResponse struct {
	Code          int    `json:"code"`
	Message       string `json:"message"`
	RequestID     string `json:"request_id"`
	Result        string `json:"result,omitempty"`
	AudioDuration int    `json:"audio_duration,omitempty"` // ms
	Language      string `json:"language,omitempty"`
	LanguageB47   string `json:"language_b47,omitempty"`
	WordSize      int    `json:"word_size,omitempty"`
	WordList      []Word `json:"word_list,omitempty"`
}

// CreateTranscriptionResponse is the flat /v3/create_transcription response.
type CreateTranscriptionResponse struct {
	Code            int    `json:"code"`
	Message         string `json:"message"`
	RequestID       string `json:"request_id"`
	TranscriptionID string `json:"transcription_id,omitempty"`
}

// SentenceDetail is one sentence of a describe_transcription result.
type SentenceDetail struct {
	FinalSentence   string  `json:"final_sentence"`
	SliceSentence   string  `json:"slice_sentence,omitempty"`
	WrittenText     string  `json:"written_text,omitempty"`
	StartMs         int     `json:"start_ms"`
	EndMs           int     `json:"end_ms"`
	WordsNum        int     `json:"words_num,omitempty"`
	Words           []Word  `json:"words,omitempty"`
	SpeechSpeed     float64 `json:"speech_speed,omitempty"`
	SpeakerID       int     `json:"speaker_id,omitempty"`
	ChannelID       int     `json:"channel_id,omitempty"`
	SpeakerRoleName string  `json:"speaker_role_name,omitempty"`
	SilenceTime     int     `json:"silence_time,omitempty"`
	Language        string  `json:"language,omitempty"`
	LanguageB47     string  `json:"language_b47,omitempty"`
}

// TranscriptionStatus is the flat /v3/describe_transcription response.
type TranscriptionStatus struct {
	Code            int              `json:"code"`
	Message         string           `json:"message"`
	RequestID       string           `json:"request_id"`
	TranscriptionID string           `json:"transcription_id,omitempty"`
	Status          int              `json:"status"`
	StatusStr       string           `json:"status_str"`
	Progress        int              `json:"progress,omitempty"`
	AudioDuration   float64          `json:"audio_duration,omitempty"` // seconds
	Result          string           `json:"result,omitempty"`
	ResultDetail    []SentenceDetail `json:"result_detail,omitempty"`
	ErrorMsg        string           `json:"error_msg,omitempty"`
}

// serverError converts a v3 flat error into an ASRError carrying the server
// code. Server codes (4xxx client errors / 5xxx server errors) are disjoint
// from the SDK-local 10xx codes, so callers can distinguish them.
func serverError(code int, message, requestID string) error {
	if requestID != "" {
		return common.NewASRErrorf(code, "%s (request_id: %s)", message, requestID)
	}
	return common.NewASRError(code, message)
}

func intPtr(v int) *int { return &v }

// itoa formats the credential's SdkAppID; the v3 auth block carries it as a
// string.
func itoa(v int) string { return fmt.Sprintf("%d", v) }
