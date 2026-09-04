package asr

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

func newTestCredential() *common.Credential {
	return common.NewCredential(12345, 1400000000, "test-secret-key-for-unit-tests")
}

func TestSentenceRecognizer_ValidateRequest(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)

	tests := []struct {
		name    string
		req     *SentenceRecognitionRequest
		wantErr string
	}{
		{
			name:    "nil request",
			req:     nil,
			wantErr: "request is nil",
		},
		{
			name:    "missing EngServiceType",
			req:     &SentenceRecognitionRequest{VoiceFormat: "pcm"},
			wantErr: "EngServiceType is required",
		},
		{
			name:    "missing VoiceFormat",
			req:     &SentenceRecognitionRequest{EngServiceType: "16k_zh"},
			wantErr: "VoiceFormat is required",
		},
		{
			name: "SourceType URL but no URL",
			req: &SentenceRecognitionRequest{
				EngServiceType: "16k_zh",
				VoiceFormat:    "wav",
				SourceType:     SourceTypeURL,
			},
			wantErr: "Url is required",
		},
		{
			name: "SourceType Data but no Data",
			req: &SentenceRecognitionRequest{
				EngServiceType: "16k_zh",
				VoiceFormat:    "pcm",
				SourceType:     SourceTypeData,
			},
			wantErr: "Data is required",
		},
		{
			name: "valid URL request",
			req: &SentenceRecognitionRequest{
				EngServiceType: "16k_zh",
				VoiceFormat:    "wav",
				SourceType:     SourceTypeURL,
				Url:            "https://example.com/test.wav",
			},
			wantErr: "",
		},
		{
			name: "valid Data request",
			req: &SentenceRecognitionRequest{
				EngServiceType: "16k_zh",
				VoiceFormat:    "pcm",
				SourceType:     SourceTypeData,
				Data:           base64.StdEncoding.EncodeToString([]byte("fake-audio")),
				DataLen:        10,
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := recognizer.validateRequest(tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("expected error containing %q, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestSentenceRecognizer_RecognizeData_EmptyData(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)

	_, err := recognizer.RecognizeData(nil, "pcm", "16k_zh")
	if err == nil {
		t.Fatal("expected error for empty data")
	}
	if !strings.Contains(err.Error(), "audio data is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSentenceRecognizer_RecognizeData_TooLarge(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)

	bigData := make([]byte, 4*1024*1024) // 4MB
	_, err := recognizer.RecognizeData(bigData, "pcm", "16k_zh")
	if err == nil {
		t.Fatal("expected error for oversized data")
	}
	if !strings.Contains(err.Error(), "3MB") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSentenceRecognizer_RecognizeURL_EmptyURL(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)

	_, err := recognizer.RecognizeURL("", "wav", "16k_zh")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "audio URL is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSentenceRecognizer_Recognize_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify method and path
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/SentenceRecognition") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		// Verify headers
		if r.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("unexpected content-type: %s", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-TRTC-SdkAppId") == "" {
			t.Error("missing X-TRTC-SdkAppId header")
		}
		if r.Header.Get("X-TRTC-UserSig") == "" {
			t.Error("missing X-TRTC-UserSig header")
		}

		// Verify query parameters
		q := r.URL.Query()
		if q.Get("AppId") == "" {
			t.Error("missing AppId query param")
		}
		if q.Get("Secretid") == "" {
			t.Error("missing Secretid query param")
		}
		if q.Get("RequestId") == "" {
			t.Error("missing RequestId query param")
		}
		if q.Get("Timestamp") == "" {
			t.Error("missing Timestamp query param")
		}

		// Verify request body
		var reqBody SentenceRecognitionRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if reqBody.EngServiceType != "16k_zh_en" {
			t.Errorf("unexpected engine type: %s", reqBody.EngServiceType)
		}

		// Return success response
		resp := SentenceRecognitionResponse{
			Response: &SentenceRecognitionResult{
				Result:        "今天天气不错",
				AudioDuration: 1500,
				WordSize:      4,
				RequestId:     "test-request-id",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	audioData := []byte("fake-pcm-audio-data")
	result, err := recognizer.RecognizeData(audioData, "pcm", "16k_zh_en")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Result != "今天天气不错" {
		t.Errorf("unexpected result: %s", result.Result)
	}
	if result.AudioDuration != 1500 {
		t.Errorf("unexpected audio duration: %d", result.AudioDuration)
	}
	if result.RequestId != "test-request-id" {
		t.Errorf("unexpected request id: %s", result.RequestId)
	}
}

func TestSentenceRecognizer_Recognize_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"Response": map[string]interface{}{
				"Error": map[string]string{
					"Code":    "4002",
					"Message": "鉴权失败",
				},
				"RequestId": "test-request-id",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	audioData := []byte("fake-pcm-audio-data")
	_, err := recognizer.RecognizeData(audioData, "pcm", "16k_zh_en")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
	if !strings.Contains(err.Error(), "4002") {
		t.Errorf("expected error code 4002, got: %v", err)
	}
	if !strings.Contains(err.Error(), "鉴权失败") {
		t.Errorf("expected error message about auth, got: %v", err)
	}
}

func TestSentenceRecognizer_Recognize_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	audioData := []byte("fake-pcm-audio-data")
	_, err := recognizer.RecognizeData(audioData, "pcm", "16k_zh_en")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestSentenceRecognizer_RecognizeDataWithOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody SentenceRecognitionRequest
		json.NewDecoder(r.Body).Decode(&reqBody)

		if reqBody.WordInfo != 2 {
			t.Errorf("expected WordInfo=2, got %d", reqBody.WordInfo)
		}
		if reqBody.FilterDirty != 1 {
			t.Errorf("expected FilterDirty=1, got %d", reqBody.FilterDirty)
		}

		resp := SentenceRecognitionResponse{
			Response: &SentenceRecognitionResult{
				Result:        "测试结果",
				AudioDuration: 1000,
				RequestId:     "test-id",
				WordSize:      2,
				WordList: []SentenceWord{
					{Word: "测试", StartTime: 0, EndTime: 500},
					{Word: "结果", StartTime: 500, EndTime: 1000},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewSentenceRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	req := &SentenceRecognitionRequest{
		EngServiceType: "16k_zh_en",
		VoiceFormat:    "pcm",
		WordInfo:       2,
		FilterDirty:    1,
	}

	result, err := recognizer.RecognizeDataWithOptions([]byte("fake-audio"), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Result != "测试结果" {
		t.Errorf("unexpected result: %s", result.Result)
	}
	if len(result.WordList) != 2 {
		t.Errorf("expected 2 words, got %d", len(result.WordList))
	}
	if result.WordList[0].Word != "测试" {
		t.Errorf("unexpected first word: %s", result.WordList[0].Word)
	}
}
