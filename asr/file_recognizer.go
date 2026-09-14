// file_recognizer.go provides an async audio file recognition client.
//
// Unlike SentenceRecognizer (one-shot, ≤60s), FileRecognizer handles longer audio
// files via an async workflow: submit a task (CreateRecTask), then poll for results
// (DescribeTaskStatus).
//
// Usage:
//
//	credential := common.NewCredential(appID, sdkAppID, secretKey)
//	recognizer := asr.NewFileRecognizer(credential)
//
//	// Submit from local file
//	taskID, err := recognizer.CreateTaskFromData(data, "pcm", "16k_zh_en")
//
//	// Poll for result
//	result, err := recognizer.WaitForResult(taskID)
package asr

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
	"github.com/google/uuid"
)

// FileEndpoint is the production HTTPS endpoint for audio file recognition.
const FileEndpoint = "https://asr.cloud-rtc.com"

// TaskStatus represents the status of an async recognition task.
const (
	TaskStatusWaiting = 0 // Task is queued
	TaskStatusRunning = 1 // Task is being processed
	TaskStatusSuccess = 2 // Task completed successfully
	TaskStatusFailed  = 3 // Task failed
)

// CreateRecTaskRequest represents the JSON request body for creating a file recognition task.
type CreateRecTaskRequest struct {
	// EngineModelType is the engine model type. Required.
	// Supported: "16k_zh" (Chinese), "16k_zh_en" (Chinese-English)
	EngineModelType string `json:"EngineModelType"`

	// ChannelNum is the number of audio channels. Required. Currently only 1 is supported.
	ChannelNum int `json:"ChannelNum"`

	// ResTextFormat controls the recognition result format.
	// 0: basic result (word count and duration only)
	// 1: detailed result with word-level timing (no punctuation timing)
	// 2: detailed result with word-level and punctuation timing
	ResTextFormat int `json:"ResTextFormat"`

	// SourceType indicates the audio data source. Required.
	// 0: audio from URL, 1: audio data in request body (base64)
	SourceType int `json:"SourceType"`

	// Url is the audio file URL (required when SourceType=0).
	// Audio duration must not exceed 12h, file size must not exceed 1GB.
	Url string `json:"Url,omitempty"`

	// Data is the base64-encoded audio data (required when SourceType=1).
	// File size must not exceed 5MB (before encoding).
	Data string `json:"Data,omitempty"`

	// DataLen is the audio data length in bytes (required when SourceType=1).
	DataLen int `json:"DataLen,omitempty"`

	// CallbackUrl is the callback URL for receiving results.
	// If set, results will be POSTed to this URL when the task completes.
	CallbackUrl string `json:"CallbackUrl,omitempty"`

	// FilterDirty controls profanity filtering (Chinese only).
	// 0: no filter (default), 1: filter, 2: replace with *
	FilterDirty int `json:"FilterDirty,omitempty"`

	// FilterModal controls modal particle filtering (Chinese only).
	// 0: no filter (default), 1: partial, 2: strict
	FilterModal int `json:"FilterModal,omitempty"`

	// FilterPunc controls sentence-ending punctuation filtering (Chinese only).
	// 0: no filter (default), 1: filter trailing punctuation, 2: filter all
	FilterPunc int `json:"FilterPunc,omitempty"`

	// ConvertNumMode controls Arabic numeral conversion.
	// 0: no conversion, 1: smart conversion (default)
	ConvertNumMode int `json:"ConvertNumMode,omitempty"`

	// HotwordId is the hotword vocabulary ID from the console.
	HotwordId string `json:"HotwordId,omitempty"`

	// HotwordList is a temporary inline hotword list.
	// Format: "word1|weight1,word2|weight2"
	HotwordList string `json:"HotwordList,omitempty"`

	// CustomizationId is the custom language model ID.
	CustomizationId string `json:"CustomizationId,omitempty"`

	// ReplaceTextId is the replacement word table ID used for forced text
	// replacement on the recognized result.
	ReplaceTextId string `json:"ReplaceTextId,omitempty"`

	// Language forces the audio language on engines that support it
	// (e.g. bigmodel). Empty means automatic detection.
	Language string `json:"Language,omitempty"`

	// SpeakerDiarization enables speaker diarization (说话人分离).
	// 0: off (default), 1: anonymous clustering, 3: voiceprint role authentication.
	//
	// When enabled, each sentence in ResultDetail carries SpeakerId; with mode 3
	// and SpeakerRoles / VoiceprintIds supplied, SpeakerRoleName is filled too.
	//
	// For stereo recordings (ChannelNum=2) do NOT enable this: the server
	// auto-fills ChannelId (1=left, 2=right) per sentence instead.
	SpeakerDiarization int `json:"SpeakerDiarization,omitempty"`

	// SpeakerNumber hints the expected number of speakers.
	// 0: auto detection (default). Only used when SpeakerDiarization > 0.
	SpeakerNumber int `json:"SpeakerNumber,omitempty"`

	// SpeakerRoles registers temporary voiceprints (enrollment audio URL +
	// role name) for this task. Only used when SpeakerDiarization is 3.
	SpeakerRoles []SpeakerRole `json:"SpeakerRoles,omitempty"`

	// VoiceprintIds lists previously enrolled voiceprint IDs.
	// Only used when SpeakerDiarization is 3.
	VoiceprintIds []string `json:"VoiceprintIds,omitempty"`

	// VadSilenceMs is the silence detection threshold in milliseconds.
	VadSilenceMs int `json:"VadSilenceMs,omitempty"`

	// VadLevel selects the VAD profile: 0 = high recall (default),
	// 1 = far-field noise filtering. It is a pointer so that an explicit 0 can
	// be distinguished from "not configured".
	VadLevel *int `json:"VadLevel,omitempty"`

	// NoiseThreshold fine-tunes VAD noise suppression, range [0, 4]. When set
	// it overrides the profile selected by VadLevel. It is a pointer because 0
	// is a valid, meaningful threshold.
	NoiseThreshold *float64 `json:"NoiseThreshold,omitempty"`
}

// CreateRecTaskResponse represents the JSON response from CreateRecTask.
type CreateRecTaskResponse struct {
	Response struct {
		Data      *CreateRecTaskData `json:"Data,omitempty"`
		RequestId string             `json:"RequestId"`
		Error     *apiError          `json:"Error,omitempty"`
	} `json:"Response"`
}

// CreateRecTaskData contains the task creation result.
type CreateRecTaskData struct {
	RecTaskId string `json:"RecTaskId"`
}

// DescribeTaskStatusRequest represents the JSON request body for querying task status.
type DescribeTaskStatusRequest struct {
	RecTaskId string `json:"RecTaskId"`
}

// DescribeTaskStatusResponse represents the JSON response from DescribeTaskStatus.
type DescribeTaskStatusResponse struct {
	Response struct {
		Data      *TaskStatus `json:"Data,omitempty"`
		RequestId string      `json:"RequestId"`
		Error     *apiError   `json:"Error,omitempty"`
	} `json:"Response"`
}

// TaskStatus contains the full task status and result.
type TaskStatus struct {
	RecTaskId     string           `json:"RecTaskId"`
	Status        int              `json:"Status"`
	StatusStr     string           `json:"StatusStr"`
	Progress      int              `json:"Progress"`
	Result        string           `json:"Result"`
	ErrorMsg      string           `json:"ErrorMsg"`
	ResultDetail  []SentenceDetail `json:"ResultDetail"`
	AudioDuration float64          `json:"AudioDuration"`
}

// SentenceDetail contains sentence-level recognition result with word timing.
type SentenceDetail struct {
	FinalSentence string          `json:"FinalSentence"`
	SliceSentence string          `json:"SliceSentence"`
	WrittenText   string          `json:"WrittenText"`
	StartMs       int             `json:"StartMs"`
	EndMs         int             `json:"EndMs"`
	WordsNum      int             `json:"WordsNum"`
	Words         []SentenceWords `json:"Words"`
	SpeechSpeed   float64         `json:"SpeechSpeed"`
	SilenceTime   int             `json:"SilenceTime"`

	// SpeakerId is the speaker number of this sentence, returned when
	// SpeakerDiarization is enabled.
	SpeakerId int `json:"SpeakerId,omitempty"`

	// SpeakerRoleName is the enrolled role name of this sentence, returned when
	// SpeakerDiarization=3 matched one of the requested SpeakerRoles /
	// VoiceprintIds. Empty when no enrolled speaker matched.
	SpeakerRoleName string `json:"SpeakerRoleName,omitempty"`

	// ChannelId is the audio channel of this sentence for stereo recordings
	// (ChannelNum=2): 1=left, 2=right. Prefer it over SpeakerId in that case.
	ChannelId int `json:"ChannelId,omitempty"`

	// Language is the detected language of this sentence, when the engine
	// reports one.
	Language string `json:"Language,omitempty"`
}

// SentenceWords contains word-level timing information within a sentence.
//
// The offsets are milliseconds relative to the start of the audio. On the wire
// the server spells them StartTime / EndTime; the older OffsetStartMs /
// OffsetEndMs spelling is accepted as a fallback.
type SentenceWords struct {
	Word          string `json:"Word"`
	OffsetStartMs int    `json:"OffsetStartMs"`
	OffsetEndMs   int    `json:"OffsetEndMs"`
}

// UnmarshalJSON accepts both wire spellings of the word offsets.
func (w *SentenceWords) UnmarshalJSON(data []byte) error {
	var raw struct {
		Word          string `json:"Word"`
		StartTime     *int   `json:"StartTime"`
		EndTime       *int   `json:"EndTime"`
		OffsetStartMs *int   `json:"OffsetStartMs"`
		OffsetEndMs   *int   `json:"OffsetEndMs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	w.Word = raw.Word
	w.OffsetStartMs = firstInt(raw.StartTime, raw.OffsetStartMs)
	w.OffsetEndMs = firstInt(raw.EndTime, raw.OffsetEndMs)
	return nil
}

// firstInt returns the first non-nil value, so an explicit zero in the primary
// key is not overridden by the fallback.
func firstInt(primary, fallback *int) int {
	if primary != nil {
		return *primary
	}
	if fallback != nil {
		return *fallback
	}
	return 0
}

// apiError is used to parse error responses from the server.
type apiError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// FileRecognizer is the client for async audio file recognition.
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

// CreateTask submits an audio file recognition task and returns the task ID.
func (r *FileRecognizer) CreateTask(req *CreateRecTaskRequest) (string, error) {
	if err := r.validateCreateRequest(req); err != nil {
		return "", err
	}

	respBody, err := r.doRequest("/v1/CreateRecTask", req)
	if err != nil {
		return "", err
	}

	var resp CreateRecTaskResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", common.NewASRErrorf(common.ErrCodeReadFailed, "unmarshal response failed: %v", err)
	}

	if resp.Response.Error != nil {
		return "", common.NewASRErrorf(common.ErrCodeServerError,
			"server error [%s]: %s (RequestId: %s)",
			resp.Response.Error.Code,
			resp.Response.Error.Message,
			resp.Response.RequestId,
		)
	}

	if resp.Response.Data == nil || resp.Response.Data.RecTaskId == "" {
		return "", common.NewASRError(common.ErrCodeServerError, "empty RecTaskId in response")
	}

	return resp.Response.Data.RecTaskId, nil
}

// CreateTaskFromData is a convenience method that submits local audio data for recognition.
// It handles base64 encoding automatically. File size must not exceed 5MB.
func (r *FileRecognizer) CreateTaskFromData(data []byte, voiceFormat, engineModelType string) (string, error) {
	if len(data) == 0 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(data) > 5*1024*1024 {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 5MB limit")
	}

	req := &CreateRecTaskRequest{
		EngineModelType: engineModelType,
		ChannelNum:      1,
		ResTextFormat:   1,
		SourceType:      SourceTypeData,
		Data:            base64.StdEncoding.EncodeToString(data),
		DataLen:         len(data),
	}
	return r.CreateTask(req)
}

// CreateTaskFromURL is a convenience method that submits an audio URL for recognition.
// Audio duration must not exceed 12h, file size must not exceed 1GB.
func (r *FileRecognizer) CreateTaskFromURL(audioURL, engineModelType string) (string, error) {
	if audioURL == "" {
		return "", common.NewASRError(common.ErrCodeInvalidParam, "audio URL is empty")
	}

	req := &CreateRecTaskRequest{
		EngineModelType: engineModelType,
		ChannelNum:      1,
		ResTextFormat:   1,
		SourceType:      SourceTypeURL,
		Url:             audioURL,
	}
	return r.CreateTask(req)
}

// CreateTaskFromDataWithOptions submits local audio data with a pre-configured request.
// It handles base64 encoding automatically. The Data, DataLen and SourceType fields will be set from rawData.
func (r *FileRecognizer) CreateTaskFromDataWithOptions(rawData []byte, req *CreateRecTaskRequest) (string, error) {
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

// DescribeTaskStatus queries the status of a file recognition task.
func (r *FileRecognizer) DescribeTaskStatus(recTaskId string) (*TaskStatus, error) {
	if recTaskId == "" {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "RecTaskId is empty")
	}

	body := &DescribeTaskStatusRequest{RecTaskId: recTaskId}

	respBody, err := r.doRequest("/v1/DescribeTaskStatus", body)
	if err != nil {
		return nil, err
	}

	var resp DescribeTaskStatusResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeReadFailed, "unmarshal response failed: %v", err)
	}

	if resp.Response.Error != nil {
		return nil, common.NewASRErrorf(common.ErrCodeServerError,
			"server error [%s]: %s (RequestId: %s)",
			resp.Response.Error.Code,
			resp.Response.Error.Message,
			resp.Response.RequestId,
		)
	}

	if resp.Response.Data == nil {
		return nil, common.NewASRError(common.ErrCodeServerError, "empty response from server")
	}

	return resp.Response.Data, nil
}

// WaitForResult polls for the result of a file recognition task until it completes or fails.
// It returns the final TaskStatus. Default poll interval is 1 second, max wait is 10 minutes.
func (r *FileRecognizer) WaitForResult(recTaskId string) (*TaskStatus, error) {
	return r.WaitForResultWithInterval(recTaskId, time.Second, 10*time.Minute)
}

// WaitForResultWithInterval polls for the result with custom interval and timeout.
func (r *FileRecognizer) WaitForResultWithInterval(recTaskId string, interval, timeout time.Duration) (*TaskStatus, error) {
	deadline := time.Now().Add(timeout)

	for {
		status, err := r.DescribeTaskStatus(recTaskId)
		if err != nil {
			return nil, err
		}

		switch status.Status {
		case TaskStatusSuccess:
			return status, nil
		case TaskStatusFailed:
			return nil, common.NewASRErrorf(common.ErrCodeServerError,
				"task failed: %s (RecTaskId: %s)", status.ErrorMsg, status.RecTaskId)
		}

		if time.Now().After(deadline) {
			return nil, common.NewASRErrorf(common.ErrCodeTimeout,
				"task not completed within %v (RecTaskId: %s, Status: %s)",
				timeout, recTaskId, status.StatusStr)
		}

		time.Sleep(interval)
	}
}

// doRequest sends an HTTP POST to the given API path with JSON body and returns the response body.
func (r *FileRecognizer) doRequest(path string, body interface{}) ([]byte, error) {
	requestID := uuid.New().String()

	userSig := r.credential.UserSig
	if userSig == "" {
		var err error
		userSig, err = common.GenUserSig(r.credential.SdkAppID, r.credential.SecretKey, requestID, 86400)
		if err != nil {
			return nil, common.NewASRErrorf(common.ErrCodeAuthFailed, "generate user sig failed: %v", err)
		}
	}

	base, err := common.ResolveHTTPEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	reqURL := fmt.Sprintf("%s%s?AppId=%d&Secretid=%d&RequestId=%s&Timestamp=%d&%s",
		base,
		path,
		r.credential.AppID,
		r.credential.AppID, // Secretid uses AppID per protocol
		requestID,
		time.Now().Unix(),
		common.SDKReportQuery(),
	)

	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeInvalidParam, "marshal request failed: %v", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeInvalidParam, "create http request failed: %v", err)
	}

	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("X-TRTC-SdkAppId", fmt.Sprintf("%d", r.credential.SdkAppID))
	httpReq.Header.Set("X-TRTC-UserSig", userSig)

	httpResp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeConnectFailed, "http request failed: %v", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeReadFailed, "read response body failed: %v", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, common.NewASRErrorf(common.ErrCodeServerError,
			"http status %d: %s", httpResp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (r *FileRecognizer) validateCreateRequest(req *CreateRecTaskRequest) error {
	if req == nil {
		return common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if req.EngineModelType == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "EngineModelType is required")
	}
	if req.ChannelNum <= 0 {
		return common.NewASRError(common.ErrCodeInvalidParam, "ChannelNum must be positive")
	}
	if req.SourceType == SourceTypeURL && req.Url == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "Url is required when SourceType=0")
	}
	if req.SourceType == SourceTypeData && req.Data == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "Data is required when SourceType=1")
	}
	if err := validateSpeakerDiarization(req.SpeakerDiarization, req.SpeakerNumber, req.SpeakerRoles, req.VoiceprintIds); err != nil {
		return err
	}
	return validateVadTuning(req.VadLevel, req.NoiseThreshold)
}
