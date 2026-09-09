// file_recognizer.go implements the v3 async audio file recognition client.
//
// Unlike SentenceRecognizer (one-shot, ≤60s), FileRecognizer handles longer
// audio via an async workflow: submit a task (/v3/create_transcription), then
// poll for results (/v3/describe_transcription).
//
// Usage:
//
//	credential := v3.NewCredential(sdkAppID, secretKey)
//	recognizer := v3.NewFileRecognizer(credential)
//	taskID, err := recognizer.CreateTaskFromData(data, "pcm", "16k_zh_en")
//	result, err := recognizer.WaitForResult(taskID)
package v3

import (
	"encoding/base64"
	"net/http"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// FileEndpoint is the production HTTPS endpoint for audio file recognition.
const FileEndpoint = "https://asr.cloud-rtc.com"

// CreateTranscriptionRequest is the params block of /v3/create_transcription.
// Field names map one-to-one to the snake_case wire names.
type CreateTranscriptionRequest struct {
	// EngineModelType is the engine model type. Required.
	EngineModelType string `json:"engine_model_type"`

	// ChannelNum is the number of audio channels. Required.
	// 1: mono, 2: stereo (8k telephony; sentences are attributed by channel_id
	// instead of speaker diarization — do not enable SpeakerDiarization).
	ChannelNum int `json:"channel_num"`

	// ResTextFormat controls the recognition result format. Required.
	// 0: basic, 1: word-level timing, 2: word-level + punctuation timing
	ResTextFormat int `json:"res_text_format"`

	// SourceType indicates the audio data source. Required.
	// 0: audio from URL, 1: audio data in request body (base64)
	SourceType int `json:"source_type"`

	// URL is the audio file URL (required when SourceType=0).
	// Audio duration must not exceed 12h, file size must not exceed 1GB.
	URL string `json:"url,omitempty"`

	// Data is the base64-encoded audio data (required when SourceType=1).
	// File size must not exceed 5MB (before encoding).
	Data string `json:"data,omitempty"`

	// DataLen is the audio data length in bytes before encoding.
	DataLen int `json:"data_len,omitempty"`

	// AudioURLs lists the pieces of a distributed recording task
	// (distributed speaker feature extraction). When non-empty, the task runs
	// the distributed path and SourceType/URL/Data must be left at their zero
	// values (SourceType=0, URL/Data empty) — the server rejects any
	// combination of AudioURLs with a single-audio source.
	AudioURLs []AudioURLItem `json:"audio_urls,omitempty"`

	// CallbackURL receives the result POST when the task completes.
	CallbackURL string `json:"callback_url,omitempty"`

	// SpeakerDiarization enables speaker diarization:
	// 0: off (default), 1: anonymous clustering, 3: voiceprint role authentication.
	SpeakerDiarization int `json:"speaker_diarization,omitempty"`

	// SpeakerNumber hints the expected number of speakers. 0: auto (default).
	SpeakerNumber int `json:"speaker_number,omitempty"`

	// VoiceprintIDs lists previously enrolled voiceprint IDs (mode 3 only).
	VoiceprintIDs []string `json:"voiceprint_ids,omitempty"`

	// SpeakerRoles registers temporary voiceprints (mode 3 only).
	SpeakerRoles []SpeakerRole `json:"speaker_roles,omitempty"`

	// HotwordID is the hotword vocabulary ID (sdkappid-scoped v3 vocabulary).
	HotwordID string `json:"hotword_id,omitempty"`

	// CustomizationID is the custom language model ID.
	CustomizationID string `json:"customization_id,omitempty"`

	// HotwordList is a temporary inline hotword list ("word|weight,...").
	HotwordList string `json:"hotword_list,omitempty"`

	// KeyWordLibIDList lists keyword library IDs.
	KeyWordLibIDList []string `json:"keyword_lib_id_list,omitempty"`

	// ReplaceTextID is the replacement word table ID for forced text
	// replacement on the recognized result.
	ReplaceTextID string `json:"replace_text_id,omitempty"`

	// ConvertNumMode controls Arabic numeral conversion.
	// 0: no conversion, 1: smart conversion (default), 3: math conversion
	ConvertNumMode int `json:"convert_num_mode,omitempty"`

	// FilterDirty controls profanity filtering.
	// 0: no filter (default), 1: filter, 2: replace with *
	FilterDirty int `json:"filter_dirty,omitempty"`

	// FilterPunc controls punctuation filtering.
	// 0: no filter (default), 1: filter
	FilterPunc int `json:"filter_punc,omitempty"`

	// FilterModal controls modal particle filtering.
	// 0: no filter (default), 1: partial, 2: strict
	FilterModal int `json:"filter_modal,omitempty"`

	// SentenceMaxLength caps the length of one result sentence.
	SentenceMaxLength int `json:"sentence_max_length,omitempty"`

	// Extra is an opaque extension string passed to the engine.
	Extra string `json:"extra,omitempty"`

	// VadSilenceMs is the silence detection threshold in milliseconds.
	VadSilenceMs int `json:"vad_silence_ms,omitempty"`

	// VadLevel selects the VAD profile: 0 = high recall (default),
	// 1 = far-field noise filtering.
	VadLevel int `json:"vad_level,omitempty"`

	// NoiseThreshold fine-tunes VAD noise suppression, range [0, 4]. When set
	// it overrides the profile selected by VadLevel. It is a pointer because 0
	// is a valid, meaningful threshold.
	NoiseThreshold *float64 `json:"noise_threshold,omitempty"`

	// Language forces the audio language on engines that support it.
	Language string `json:"language,omitempty"`

	// Context is the recognition context (background text, terms, domain
	// key-values).
	Context *Context `json:"context,omitempty"`
}

// createTranscriptionParamsWire adds the SDK self-identification to the
// params block (the worker ignores unknown keys).
type createTranscriptionParamsWire struct {
	*CreateTranscriptionRequest
	SDKInfo map[string]string `json:"sdk_info,omitempty"`
}

// describeTranscriptionRequest is the params block of
// /v3/describe_transcription.
type describeTranscriptionRequest struct {
	TranscriptionID string            `json:"transcription_id"`
	SDKInfo         map[string]string `json:"sdk_info,omitempty"`
}

// FileRecognizer is the v3 client for async audio file recognition.
type FileRecognizer struct {
	credential *common.Credential
	endpoint   string
	httpClient *http.Client
}

// NewFileRecognizer creates a new FileRecognizer.
func NewFileRecognizer(credential *common.Credential) *FileRecognizer {
	return &FileRecognizer{
		credential: credential,
		endpoint:   "",
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// SetEndpoint overrides the default API endpoint (for testing).
func (r *FileRecognizer) SetEndpoint(endpoint string) {
	r.endpoint = endpoint
}

// SetHTTPClient sets a custom HTTP client.
func (r *FileRecognizer) SetHTTPClient(client *http.Client) {
	r.httpClient = client
}

// CreateTask submits an audio file recognition task and returns the
// transcription ID (valid for 24 hours). A non-zero server code is returned
// as an error carrying that code.
func (r *FileRecognizer) CreateTask(req *CreateTranscriptionRequest) (string, error) {
	if err := r.validateCreateRequest(req); err != nil {
		return "", err
	}

	resp, err := r.createTranscription(req)
	if err != nil {
		return "", err
	}
	if resp.TranscriptionID == "" {
		return "", common.NewASRError(common.ErrCodeServerError, "empty transcription_id in response")
	}
	return resp.TranscriptionID, nil
}

// CreateTaskFromData is a convenience method that submits local audio data
// for recognition. It handles base64 encoding automatically. File size must
// not exceed 5MB.
func (r *FileRecognizer) CreateTaskFromData(data []byte, voiceFormat, engineModelType string) (string, error) {
	if len(data) == 0 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(data) > 5*1024*1024 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 5MB limit")
	}

	req := &CreateTranscriptionRequest{
		EngineModelType: engineModelType,
		ChannelNum:      1,
		ResTextFormat:   1,
		SourceType:      SourceTypeData,
		Data:            base64.StdEncoding.EncodeToString(data),
		DataLen:         len(data),
	}
	return r.CreateTask(req)
}

// CreateTaskFromURL is a convenience method that submits an audio URL for
// recognition. Audio duration must not exceed 12h, file size must not exceed
// 1GB.
func (r *FileRecognizer) CreateTaskFromURL(audioURL, engineModelType string) (string, error) {
	if audioURL == "" {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio URL is empty")
	}

	req := &CreateTranscriptionRequest{
		EngineModelType: engineModelType,
		ChannelNum:      1,
		ResTextFormat:   1,
		SourceType:      SourceTypeURL,
		URL:             audioURL,
	}
	return r.CreateTask(req)
}

// CreateTaskFromDataWithOptions submits local audio data with a
// pre-configured request. It handles base64 encoding automatically. The Data,
// DataLen and SourceType fields will be set from rawData (the request is
// mutated in place).
func (r *FileRecognizer) CreateTaskFromDataWithOptions(rawData []byte, req *CreateTranscriptionRequest) (string, error) {
	if req == nil {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if len(rawData) == 0 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(rawData) > 5*1024*1024 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 5MB limit")
	}

	req.SourceType = SourceTypeData
	req.Data = base64.StdEncoding.EncodeToString(rawData)
	req.DataLen = len(rawData)
	return r.CreateTask(req)
}

// DescribeTask queries the status of a file recognition task. Note that v1
// (RecTaskId) and v3 (transcription_id) task IDs live in separate spaces: an
// ID created by one protocol cannot be queried through the other.
func (r *FileRecognizer) DescribeTask(transcriptionID string) (*TranscriptionStatus, error) {
	if transcriptionID == "" {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "transcriptionID is empty")
	}

	body, _, err := marshalOfflineRequest(r.credential, describeTranscriptionRequest{
		TranscriptionID: transcriptionID,
		SDKInfo:         common.SDKReportParams(),
	})
	if err != nil {
		return nil, err
	}

	base, err := common.ResolveHTTPEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	respBody, status, err := postV3(r.httpClient, base+"/v3/describe_transcription", body)
	if err != nil {
		return nil, err
	}

	var resp TranscriptionStatus
	if err := decodeFlatResponse(respBody, status, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, serverError(resp.Code, resp.Message, resp.RequestID)
	}
	return &resp, nil
}

// WaitForResult polls for the result of a file recognition task until it
// completes or fails. Default poll interval is 1 second, max wait 10 minutes.
func (r *FileRecognizer) WaitForResult(transcriptionID string) (*TranscriptionStatus, error) {
	return r.WaitForResultWithInterval(transcriptionID, time.Second, 10*time.Minute)
}

// WaitForResultWithInterval polls for the result with custom interval and
// timeout.
func (r *FileRecognizer) WaitForResultWithInterval(transcriptionID string, interval, timeout time.Duration) (*TranscriptionStatus, error) {
	deadline := time.Now().Add(timeout)

	for {
		status, err := r.DescribeTask(transcriptionID)
		if err != nil {
			return nil, err
		}

		switch status.Status {
		case TaskStatusSuccess:
			return status, nil
		case TaskStatusFailed:
			return nil, common.NewASRErrorf(common.ErrCodeServerError,
				"task failed: %s (transcription_id: %s)", status.ErrorMsg, status.TranscriptionID)
		}

		if time.Now().After(deadline) {
			return nil, common.NewASRErrorf(common.ErrCodeTimeout,
				"task not completed within %v (transcription_id: %s, Status: %s)",
				timeout, transcriptionID, status.StatusStr)
		}

		time.Sleep(interval)
	}
}

// createTranscription performs the /v3/create_transcription round-trip.
func (r *FileRecognizer) createTranscription(req *CreateTranscriptionRequest) (*CreateTranscriptionResponse, error) {
	body, _, err := marshalOfflineRequest(r.credential,
		createTranscriptionParamsWire{CreateTranscriptionRequest: req, SDKInfo: common.SDKReportParams()})
	if err != nil {
		return nil, err
	}

	base, err := common.ResolveHTTPEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	respBody, status, err := postV3(r.httpClient, base+"/v3/create_transcription", body)
	if err != nil {
		return nil, err
	}

	var resp CreateTranscriptionResponse
	if err := decodeFlatResponse(respBody, status, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, serverError(resp.Code, resp.Message, resp.RequestID)
	}
	return &resp, nil
}

func (r *FileRecognizer) validateCreateRequest(req *CreateTranscriptionRequest) error {
	if req == nil {
		return common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if req.EngineModelType == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "EngineModelType is required")
	}
	if req.ChannelNum != 1 && req.ChannelNum != 2 {
		return common.NewASRError(common.ErrCodeInvalidParam, "ChannelNum must be 1 or 2")
	}
	if err := validateEnumOption("ResTextFormat", req.ResTextFormat, 0, 1, 2, 3); err != nil {
		return err
	}
	if req.ChannelNum == 2 && req.SpeakerDiarization != SpeakerDiarizationOff {
		return common.NewASRError(common.ErrCodeInvalidParam,
			"SpeakerDiarization is not supported for stereo (ChannelNum=2); sentences carry ChannelID instead")
	}
	if len(req.AudioURLs) > 0 {
		// Distributed path: the server requires SourceType=0 with URL/Data
		// empty, and each item carrying a unique non-negative Index and an
		// absolute http(s) URL (rectask_cluster.go validateDistributedAudioUrlsRequest).
		if err := validateAudioURLs(req); err != nil {
			return err
		}
	} else {
		if req.SourceType == SourceTypeURL && req.URL == "" {
			return common.NewASRError(common.ErrCodeInvalidParam, "URL is required when SourceType=0")
		}
		if req.SourceType == SourceTypeData && req.Data == "" {
			return common.NewASRError(common.ErrCodeInvalidParam, "Data is required when SourceType=1")
		}
	}
	if err := validateSpeakerDiarization(req.SpeakerDiarization, req.SpeakerNumber, req.SpeakerRoles, req.VoiceprintIDs); err != nil {
		return err
	}
	if err := validateVadTuning(intPtrOrNil(req.VadLevel), req.NoiseThreshold); err != nil {
		return err
	}
	return nil
}

// intPtrOrNil converts the plain-int VadLevel wire field to the validation
// helper's pointer form: 0 is both the default and the high-recall value, so
// it is always in range and only 1 needs an explicit check.
func intPtrOrNil(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}
