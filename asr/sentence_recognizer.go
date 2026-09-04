// sentence_recognizer.go provides a one-shot sentence recognition client.
//
// Usage:
//
//	credential := common.NewCredential(appID, sdkAppID, secretKey)
//	recognizer := asr.NewSentenceRecognizer(credential)
//	resp, err := recognizer.Recognize(request)
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

// SentenceEndpoint is the production HTTPS endpoint for sentence recognition.
const SentenceEndpoint = "https://asr.cloud-rtc.com"

// SourceType indicates the audio data source.
const (
	SourceTypeURL  = 0 // Audio from a URL
	SourceTypeData = 1 // Audio data in request body (base64 encoded)
)

// SentenceRecognitionRequest represents the JSON request body for sentence recognition.
type SentenceRecognitionRequest struct {
	// EngServiceType is the engine model type. Required.
	// Supported: "16k_zh" (Chinese), "16k_zh_en" (Chinese-English)
	EngServiceType string `json:"EngSerViceType"`

	// SourceType indicates the audio source.
	// 0: audio from URL, 1: audio data in request body (base64)
	SourceType int `json:"SourceType"`

	// VoiceFormat is the audio format.
	// Supported: "wav", "pcm", "ogg-opus", "mp3", "m4a"
	VoiceFormat string `json:"VoiceFormat"`

	// Url is the audio file URL (required when SourceType=0).
	// Audio duration must not exceed 60s, file size must not exceed 3MB.
	Url string `json:"Url,omitempty"`

	// Data is the base64-encoded audio data (required when SourceType=1).
	// Audio duration must not exceed 60s, file size must not exceed 3MB (before encoding).
	Data string `json:"Data,omitempty"`

	// DataLen is the audio data length in bytes (required when SourceType=1).
	// When SourceType=0, this can be omitted (length is determined from URL content).
	DataLen int `json:"DataLen,omitempty"`

	// WordInfo controls word-level timing display.
	// 0: hide (default), 1: show without punctuation timing, 2: show with punctuation timing
	WordInfo int `json:"WordInfo,omitempty"`

	// FilterDirty controls profanity filtering (Chinese only).
	// 0: no filter (default), 1: filter, 2: replace with *
	FilterDirty int `json:"FilterDirty,omitempty"`

	// FilterModal controls modal particle filtering (Chinese only).
	// 0: no filter (default), 1: partial, 2: strict
	FilterModal int `json:"FilterModal,omitempty"`

	// FilterPunc controls sentence-ending punctuation filtering (Chinese only).
	// 0: no filter (default), 2: filter all punctuation
	FilterPunc int `json:"FilterPunc,omitempty"`

	// ConvertNumMode controls Arabic numeral conversion.
	// 0: no conversion, 1: smart conversion (default)
	ConvertNumMode int `json:"ConvertNumMode,omitempty"`

	// HotwordID is the hotword vocabulary ID from the console.
	HotwordID string `json:"HotwordId,omitempty"`

	// HotwordList is a temporary inline hotword list.
	// Format: "word1|weight1,word2|weight2" (word max 10 chars, weight 1-11 or 100)
	HotwordList string `json:"HotwordList,omitempty"`

	// CustomizationID is the custom language model ID.
	CustomizationID string `json:"CustomizationId,omitempty"`

	// InputSampleRate overrides the engine sample rate for 8k PCM audio.
	// Only for PCM format. Supported: 8000. Used with 16k engine to upsample.
	InputSampleRate int `json:"InputSampleRate,omitempty"`

	// Language forces the audio language on engines that support it
	// (e.g. bigmodel). Empty means automatic detection.
	Language string `json:"Language,omitempty"`
}

// SentenceRecognitionResponse represents the JSON response from sentence recognition.
type SentenceRecognitionResponse struct {
	Response *SentenceRecognitionResult `json:"Response"`
}

// SentenceRecognitionResult contains the recognition result.
type SentenceRecognitionResult struct {
	// Result is the recognition text.
	Result string `json:"Result"`

	// AudioDuration is the audio duration in milliseconds.
	AudioDuration int `json:"AudioDuration"`

	// WordSize is the word count (may be 0 if WordInfo is not enabled).
	WordSize int `json:"WordSize"`

	// WordList contains word-level timing details (nil if WordInfo is not enabled).
	WordList []SentenceWord `json:"WordList"`

	// RequestId is the unique request identifier.
	RequestId string `json:"RequestId"`
}

// SentenceWord contains word-level timing information.
type SentenceWord struct {
	// Word is the recognized word text.
	Word string `json:"Word"`

	// StartTime is the word start time in milliseconds.
	StartTime int `json:"StartTime"`

	// EndTime is the word end time in milliseconds.
	EndTime int `json:"EndTime"`
}

// sentenceErrorResponse is used to parse error responses from the server.
type sentenceErrorResponse struct {
	Response struct {
		Error *struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		} `json:"Error,omitempty"`
		RequestId string `json:"RequestId"`
	} `json:"Response"`
}

// SentenceRecognizer is the client for one-shot sentence recognition.
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
func (r *SentenceRecognizer) Recognize(req *SentenceRecognitionRequest) (*SentenceRecognitionResult, error) {
	if err := r.validateRequest(req); err != nil {
		return nil, err
	}

	requestID := uuid.New().String()

	// Generate UserSig using RequestId as the userID per protocol spec
	userSig := r.credential.UserSig
	if userSig == "" {
		var err error
		userSig, err = common.GenUserSig(r.credential.SdkAppID, r.credential.SecretKey, requestID, 86400)
		if err != nil {
			return nil, common.NewASRErrorf(common.ErrCodeAuthFailed, "generate user sig failed: %v", err)
		}
	}

	// Build URL with query parameters
	base, err := common.ResolveHTTPEndpoint(r.endpoint, common.SiteOf(r.credential))
	if err != nil {
		return nil, err
	}
	reqURL := fmt.Sprintf("%s/v1/SentenceRecognition?AppId=%d&Secretid=%d&RequestId=%s&Timestamp=%d&%s",
		base,
		r.credential.AppID,
		r.credential.AppID, // Secretid uses AppID per protocol
		requestID,
		time.Now().Unix(),
		common.SDKReportQuery(),
	)

	// Marshal request body
	body, err := json.Marshal(req)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeInvalidParam, "marshal request failed: %v", err)
	}

	// Build HTTP request
	httpReq, err := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeInvalidParam, "create http request failed: %v", err)
	}

	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("X-TRTC-SdkAppId", fmt.Sprintf("%d", r.credential.SdkAppID))
	httpReq.Header.Set("X-TRTC-UserSig", userSig)

	// Execute request
	httpResp, err := r.httpClient.Do(httpReq)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeConnectFailed, "http request failed: %v", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeReadFailed, "read response body failed: %v", err)
	}

	// Check for HTTP-level errors
	if httpResp.StatusCode != http.StatusOK {
		return nil, common.NewASRErrorf(common.ErrCodeServerError,
			"http status %d: %s", httpResp.StatusCode, string(respBody))
	}

	// Check for API-level errors
	var errResp sentenceErrorResponse
	if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Response.Error != nil {
		return nil, common.NewASRErrorf(common.ErrCodeServerError,
			"server error [%s]: %s (RequestId: %s)",
			errResp.Response.Error.Code,
			errResp.Response.Error.Message,
			errResp.Response.RequestId,
		)
	}

	// Parse success response
	var resp SentenceRecognitionResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, common.NewASRErrorf(common.ErrCodeReadFailed, "unmarshal response failed: %v", err)
	}

	if resp.Response == nil {
		return nil, common.NewASRError(common.ErrCodeServerError, "empty response from server")
	}

	return resp.Response, nil
}

// RecognizeData is a convenience method that sends local audio data for recognition.
// It handles base64 encoding automatically.
func (r *SentenceRecognizer) RecognizeData(data []byte, voiceFormat, engineModelType string) (*SentenceRecognitionResult, error) {
	if len(data) == 0 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data is empty")
	}
	if len(data) > 3*1024*1024 {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio data exceeds 3MB limit")
	}

	req := &SentenceRecognitionRequest{
		EngServiceType: engineModelType,
		SourceType:     SourceTypeData,
		VoiceFormat:    voiceFormat,
		Data:           base64.StdEncoding.EncodeToString(data),
		DataLen:        len(data),
	}
	return r.Recognize(req)
}

// RecognizeURL is a convenience method that sends an audio URL for recognition.
func (r *SentenceRecognizer) RecognizeURL(audioURL, voiceFormat, engineModelType string) (*SentenceRecognitionResult, error) {
	if audioURL == "" {
		return nil, common.NewASRError(common.ErrCodeInvalidParam, "audio URL is empty")
	}

	req := &SentenceRecognitionRequest{
		EngServiceType: engineModelType,
		SourceType:     SourceTypeURL,
		VoiceFormat:    voiceFormat,
		Url:            audioURL,
	}
	return r.Recognize(req)
}

// RecognizeDataWithOptions sends local audio data with a pre-configured request.
// It handles base64 encoding automatically. The Data and DataLen fields will be set from rawData.
func (r *SentenceRecognizer) RecognizeDataWithOptions(rawData []byte, req *SentenceRecognitionRequest) (*SentenceRecognitionResult, error) {
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

func (r *SentenceRecognizer) validateRequest(req *SentenceRecognitionRequest) error {
	if req == nil {
		return common.NewASRError(common.ErrCodeInvalidParam, "request is nil")
	}
	if req.EngServiceType == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "EngServiceType is required")
	}
	if req.VoiceFormat == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "VoiceFormat is required")
	}
	if req.SourceType == SourceTypeURL && req.Url == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "Url is required when SourceType=0")
	}
	if req.SourceType == SourceTypeData && req.Data == "" {
		return common.NewASRError(common.ErrCodeInvalidParam, "Data is required when SourceType=1")
	}
	return nil
}
