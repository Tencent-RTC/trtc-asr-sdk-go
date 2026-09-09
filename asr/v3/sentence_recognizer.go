// sentence_recognizer.go implements the v3 one-shot sentence recognition
// client (POST /v3/transcribe).
//
// Usage:
//
//	credential := v3.NewCredential(sdkAppID, secretKey)
//	recognizer := v3.NewSentenceRecognizer(credential)
//	resp, err := recognizer.Recognize(request)
package v3

import (
	"encoding/base64"
	"net/http"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// SentenceEndpoint is the production HTTPS endpoint for sentence recognition.
const SentenceEndpoint = "https://asr.cloud-rtc.com"

// TranscribeRequest is the params block of /v3/transcribe. Field names map
// one-to-one to the snake_case wire names.
type TranscribeRequest struct {
	// EngineModelType is the engine model type. Required.
	// Supported: "16k_zh" (Chinese), "16k_zh_en" (Chinese-English), ...
	EngineModelType string `json:"engine_model_type"`

	// SourceType indicates the audio source. Required.
	// 0: audio from URL, 1: audio data in request body (base64)
	SourceType int `json:"source_type"`

	// VoiceFormat is the audio format. Required.
	// Supported: "wav", "pcm", "ogg-opus", "mp3", "m4a"
	VoiceFormat string `json:"voice_format"`

	// URL is the audio file URL (required when SourceType=0).
	// Audio duration must not exceed 60s, file size must not exceed 3MB.
	URL string `json:"url,omitempty"`

	// Data is the base64-encoded audio data (required when SourceType=1).
	Data string `json:"data,omitempty"`

	// DataLen is the audio data length in bytes before encoding
	// (required when SourceType=1).
	DataLen int `json:"data_len,omitempty"`

	// WordInfo controls word-level timing display.
	// 0: hide (default), 1: show without punctuation timing, 2: with punctuation
	WordInfo int `json:"word_info,omitempty"`

	// FilterDirty controls profanity filtering (Chinese only).
	// 0: no filter (default), 1: filter, 2: replace with *
	FilterDirty int `json:"filter_dirty,omitempty"`

	// FilterModal controls modal particle filtering (Chinese only).
	// 0: no filter (default), 1: partial, 2: strict
	FilterModal int `json:"filter_modal,omitempty"`

	// FilterPunc controls sentence-ending punctuation filtering (Chinese only).
	// 0: no filter (default), 1: filter
	FilterPunc int `json:"filter_punc,omitempty"`

	// ConvertNumMode controls Arabic numeral conversion.
	// 0: no conversion, 1: smart conversion (default), 3: math conversion
	ConvertNumMode int `json:"convert_num_mode,omitempty"`

	// HotwordID is the hotword vocabulary ID (sdkappid-scoped v3 vocabulary).
	HotwordID string `json:"hotword_id,omitempty"`

	// CustomizationID is the custom language model ID.
	CustomizationID string `json:"customization_id,omitempty"`

	// HotwordList is a temporary inline hotword list.
	// Format: "word1|weight1,word2|weight2" (word max 30 chars, weight 1-11 or 100)
	HotwordList string `json:"hotword_list,omitempty"`

	// InputSampleRate overrides the engine sample rate for 8k PCM audio.
	// Only for PCM format. Supported: 8000. Used with 16k engine to upsample.
	InputSampleRate int `json:"input_sample_rate,omitempty"`

	// Needvad controls VAD. nil leaves the server default; an explicit 0/1 is
	// honored (pointer semantics).
	Needvad *int `json:"needvad,omitempty"`

	// VadSilenceTimeMs is the silence detection threshold in milliseconds
	// (240-2000 with VAD enabled). nil leaves the server default (800).
	VadSilenceTimeMs *int `json:"vad_silence_time,omitempty"`

	// Language forces the audio language on engines that support it
	// (e.g. bigmodel). Empty means automatic detection.
	Language string `json:"language,omitempty"`

	// SpeakerDiarization enables speaker diarization:
	// 0: off (default), 1: anonymous clustering, 3: voiceprint role authentication.
	SpeakerDiarization int `json:"speaker_diarization,omitempty"`

	// SpeakerNumber hints the expected number of speakers. 0: auto (default).
	SpeakerNumber int `json:"speaker_number,omitempty"`

	// Context is the recognition context (background text, terms, domain
	// key-values). LLM-class engines consume it; traditional engines degrade
	// Context.Terms to hotwords.
	Context *Context `json:"context,omitempty"`
}

// transcribeParamsWire adds the SDK self-identification to the params block.
// The worker ignores unknown keys, so the telemetry survives in server-side
// dumps without disturbing the protocol.
type transcribeParamsWire struct {
	*TranscribeRequest
	SDKInfo map[string]string `json:"sdk_info,omitempty"`
}

// SentenceRecognizer is the v3 client for one-shot sentence recognition.
type SentenceRecognizer struct {
	credential *common.Credential
	endpoint   string
	httpClient *http.Client
}

// NewSentenceRecognizer creates a new SentenceRecognizer.
func NewSentenceRecognizer(credential *common.Credential) *SentenceRecognizer {
	return &SentenceRecognizer{
		credential: credential,
		endpoint:   "",
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// SetEndpoint overrides the default API endpoint (for testing).
func (r *SentenceRecognizer) SetEndpoint(endpoint string) {
	r.endpoint = endpoint
}

// SetHTTPClient sets a custom HTTP client.
func (r *SentenceRecognizer) SetHTTPClient(client *http.Client) {
	r.httpClient = client
}

// Recognize sends a sentence recognition request and returns the result.
// A non-zero server code is returned as an error carrying that code.
func (r *SentenceRecognizer) Recognize(req *TranscribeRequest) (*TranscribeResponse, error) {
	if err := r.validateRequest(req); err != nil {
		return nil, err
	}

	body, _, err := marshalOfflineRequest(r.credential,
		transcribeParamsWire{TranscribeRequest: req, SDKInfo: common.SDKReportParams()})
	if err != nil {
		return nil, err
	}

	base, err := common.ResolveHTTPEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	respBody, status, err := postV3(r.httpClient, base+"/v3/transcribe", body)
	if err != nil {
		return nil, err
	}

	var resp TranscribeResponse
	if err := decodeFlatResponse(respBody, status, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, serverError(resp.Code, resp.Message, resp.RequestID)
	}
	return &resp, nil
}

// RecognizeData is a convenience method that sends local audio data for
// recognition. It handles base64 encoding automatically.
// Audio duration must not exceed 60s, data size must not exceed 3MB.
func (r *SentenceRecognizer) RecognizeData(data []byte, voiceFormat, engineModelType string) (*TranscribeResponse, error) {
	if len(data) == 0 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(data) > 3*1024*1024 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 3MB limit")
	}

	req := &TranscribeRequest{
		EngineModelType: engineModelType,
		SourceType:      SourceTypeData,
		VoiceFormat:     voiceFormat,
		Data:            base64.StdEncoding.EncodeToString(data),
		DataLen:         len(data),
	}
	return r.Recognize(req)
}

// RecognizeURL is a convenience method that sends an audio URL for recognition.
func (r *SentenceRecognizer) RecognizeURL(audioURL, voiceFormat, engineModelType string) (*TranscribeResponse, error) {
	if audioURL == "" {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio URL is empty")
	}

	req := &TranscribeRequest{
		EngineModelType: engineModelType,
		SourceType:      SourceTypeURL,
		VoiceFormat:     voiceFormat,
		URL:             audioURL,
	}
	return r.Recognize(req)
}

// RecognizeDataWithOptions sends local audio data with a pre-configured
// request. It handles base64 encoding automatically. The Data and DataLen
// fields will be set from rawData (the request is mutated in place).
func (r *SentenceRecognizer) RecognizeDataWithOptions(rawData []byte, req *TranscribeRequest) (*TranscribeResponse, error) {
	if req == nil {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if len(rawData) == 0 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(rawData) > 3*1024*1024 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 3MB limit")
	}

	req.SourceType = SourceTypeData
	req.Data = base64.StdEncoding.EncodeToString(rawData)
	req.DataLen = len(rawData)
	return r.Recognize(req)
}

func (r *SentenceRecognizer) validateRequest(req *TranscribeRequest) error {
	if req == nil {
		return common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if req.EngineModelType == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "EngineModelType is required")
	}
	if req.VoiceFormat == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "VoiceFormat is required")
	}
	if req.SourceType == SourceTypeURL && req.URL == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "URL is required when SourceType=0")
	}
	if req.SourceType == SourceTypeData && req.Data == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "Data is required when SourceType=1")
	}
	if err := validateSpeakerDiarization(req.SpeakerDiarization, req.SpeakerNumber, nil, nil); err != nil {
		return err
	}
	if req.Needvad != nil {
		if err := validateEnumOption("Needvad", *req.Needvad, 0, 1); err != nil {
			return err
		}
	}
	// 8000 is the only supported override; 0 means "use the engine rate".
	return validateEnumOption("InputSampleRate", req.InputSampleRate, 0, 8000)
}
