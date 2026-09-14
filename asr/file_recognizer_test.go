package asr

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileRecognizer_ValidateCreateRequest(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)

	tests := []struct {
		name    string
		req     *CreateRecTaskRequest
		wantErr string
	}{
		{
			name:    "nil request",
			req:     nil,
			wantErr: "request is nil",
		},
		{
			name:    "missing EngineModelType",
			req:     &CreateRecTaskRequest{ChannelNum: 1},
			wantErr: "EngineModelType is required",
		},
		{
			name:    "invalid ChannelNum",
			req:     &CreateRecTaskRequest{EngineModelType: "16k_zh", ChannelNum: 0},
			wantErr: "ChannelNum must be positive",
		},
		{
			name: "SourceType URL but no URL",
			req: &CreateRecTaskRequest{
				EngineModelType: "16k_zh",
				ChannelNum:      1,
				SourceType:      SourceTypeURL,
			},
			wantErr: "Url is required",
		},
		{
			name: "SourceType Data but no Data",
			req: &CreateRecTaskRequest{
				EngineModelType: "16k_zh",
				ChannelNum:      1,
				SourceType:      SourceTypeData,
			},
			wantErr: "Data is required",
		},
		{
			name: "valid URL request",
			req: &CreateRecTaskRequest{
				EngineModelType: "16k_zh",
				ChannelNum:      1,
				SourceType:      SourceTypeURL,
				Url:             "https://example.com/test.wav",
			},
			wantErr: "",
		},
		{
			name: "valid Data request",
			req: &CreateRecTaskRequest{
				EngineModelType: "16k_zh",
				ChannelNum:      1,
				SourceType:      SourceTypeData,
				Data:            base64.StdEncoding.EncodeToString([]byte("fake-audio")),
				DataLen:         10,
			},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := recognizer.validateCreateRequest(tt.req)
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

func TestFileRecognizer_CreateTaskFromData_EmptyData(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)

	_, err := recognizer.CreateTaskFromData(nil, "pcm", "16k_zh")
	if err == nil {
		t.Fatal("expected error for empty data")
	}
	if !strings.Contains(err.Error(), "audio data is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileRecognizer_CreateTaskFromData_TooLarge(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)

	bigData := make([]byte, 6*1024*1024) // 6MB > 5MB limit
	_, err := recognizer.CreateTaskFromData(bigData, "pcm", "16k_zh")
	if err == nil {
		t.Fatal("expected error for oversized data")
	}
	if !strings.Contains(err.Error(), "5MB") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileRecognizer_CreateTaskFromURL_EmptyURL(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)

	_, err := recognizer.CreateTaskFromURL("", "16k_zh")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "audio URL is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileRecognizer_CreateTask_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/CreateRecTask") {
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
		var reqBody CreateRecTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		if reqBody.EngineModelType != "16k_zh_en" {
			t.Errorf("unexpected engine type: %s", reqBody.EngineModelType)
		}
		if reqBody.SourceType != SourceTypeData {
			t.Errorf("expected SourceType=1, got %d", reqBody.SourceType)
		}

		resp := CreateRecTaskResponse{}
		resp.Response.Data = &CreateRecTaskData{RecTaskId: "test-task-id-12345"}
		resp.Response.RequestId = "test-request-id"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	taskID, err := recognizer.CreateTaskFromData([]byte("fake-pcm-audio"), "pcm", "16k_zh_en")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if taskID != "test-task-id-12345" {
		t.Errorf("unexpected task ID: %s", taskID)
	}
}

func TestFileRecognizer_CreateTask_ServerError(t *testing.T) {
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
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	_, err := recognizer.CreateTaskFromData([]byte("fake-audio"), "pcm", "16k_zh_en")
	if err == nil {
		t.Fatal("expected error for server error response")
	}
	if !strings.Contains(err.Error(), "4002") {
		t.Errorf("expected error code 4002, got: %v", err)
	}
}

func TestFileRecognizer_CreateTask_HTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	_, err := recognizer.CreateTaskFromData([]byte("fake-audio"), "pcm", "16k_zh_en")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected 500 in error, got: %v", err)
	}
}

func TestFileRecognizer_DescribeTaskStatus_EmptyTaskID(t *testing.T) {
	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)

	_, err := recognizer.DescribeTaskStatus("")
	if err == nil {
		t.Fatal("expected error for empty task ID")
	}
	if !strings.Contains(err.Error(), "RecTaskId is empty") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestFileRecognizer_DescribeTaskStatus_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/DescribeTaskStatus") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var reqBody DescribeTaskStatusRequest
		json.NewDecoder(r.Body).Decode(&reqBody)
		if reqBody.RecTaskId != "task-123" {
			t.Errorf("unexpected RecTaskId: %s", reqBody.RecTaskId)
		}

		resp := DescribeTaskStatusResponse{}
		resp.Response.Data = &TaskStatus{
			RecTaskId:     "task-123",
			Status:        TaskStatusSuccess,
			StatusStr:     "success",
			Result:        "今天天气不错。",
			AudioDuration: 2.38,
			ResultDetail: []SentenceDetail{
				{
					FinalSentence: "今天天气不错。",
					SliceSentence: "今天 天气 不错",
					StartMs:       200,
					EndMs:         1380,
					WordsNum:      1,
					SpeechSpeed:   2,
				},
			},
		}
		resp.Response.RequestId = "req-123"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	status, err := recognizer.DescribeTaskStatus("task-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != TaskStatusSuccess {
		t.Errorf("expected status success, got %d", status.Status)
	}
	if status.Result != "今天天气不错。" {
		t.Errorf("unexpected result: %s", status.Result)
	}
	if status.AudioDuration != 2.38 {
		t.Errorf("unexpected audio duration: %f", status.AudioDuration)
	}
	if len(status.ResultDetail) != 1 {
		t.Fatalf("expected 1 detail, got %d", len(status.ResultDetail))
	}
	if status.ResultDetail[0].FinalSentence != "今天天气不错。" {
		t.Errorf("unexpected detail: %s", status.ResultDetail[0].FinalSentence)
	}
}

func TestFileRecognizer_DescribeTaskStatus_TaskFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DescribeTaskStatusResponse{}
		resp.Response.Data = &TaskStatus{
			RecTaskId: "task-fail",
			Status:    TaskStatusFailed,
			StatusStr: "failed",
			ErrorMsg:  "Failed to download audio file",
		}
		resp.Response.RequestId = "req-456"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	status, err := recognizer.DescribeTaskStatus("task-fail")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != TaskStatusFailed {
		t.Errorf("expected failed status, got %d", status.Status)
	}
	if status.ErrorMsg != "Failed to download audio file" {
		t.Errorf("unexpected error msg: %s", status.ErrorMsg)
	}
}

func TestFileRecognizer_WaitForResult_ImmediateSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DescribeTaskStatusResponse{}
		resp.Response.Data = &TaskStatus{
			RecTaskId:     "task-ok",
			Status:        TaskStatusSuccess,
			StatusStr:     "success",
			Result:        "识别结果",
			AudioDuration: 1.5,
		}
		resp.Response.RequestId = "req-ok"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	status, err := recognizer.WaitForResult("task-ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Result != "识别结果" {
		t.Errorf("unexpected result: %s", status.Result)
	}
}

func TestFileRecognizer_WaitForResult_PollingThenSuccess(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		resp := DescribeTaskStatusResponse{}
		if n < 3 {
			resp.Response.Data = &TaskStatus{
				RecTaskId: "task-poll",
				Status:    TaskStatusRunning,
				StatusStr: "doing",
			}
		} else {
			resp.Response.Data = &TaskStatus{
				RecTaskId:     "task-poll",
				Status:        TaskStatusSuccess,
				StatusStr:     "success",
				Result:        "轮询成功",
				AudioDuration: 3.0,
			}
		}
		resp.Response.RequestId = "req-poll"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	status, err := recognizer.WaitForResultWithInterval("task-poll", 50*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Result != "轮询成功" {
		t.Errorf("unexpected result: %s", status.Result)
	}
	if atomic.LoadInt32(&callCount) < 3 {
		t.Errorf("expected at least 3 calls, got %d", callCount)
	}
}

func TestFileRecognizer_WaitForResult_TaskFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DescribeTaskStatusResponse{}
		resp.Response.Data = &TaskStatus{
			RecTaskId: "task-err",
			Status:    TaskStatusFailed,
			StatusStr: "failed",
			ErrorMsg:  "转码失败",
		}
		resp.Response.RequestId = "req-err"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	_, err := recognizer.WaitForResult("task-err")
	if err == nil {
		t.Fatal("expected error for failed task")
	}
	if !strings.Contains(err.Error(), "转码失败") {
		t.Errorf("expected '转码失败' in error, got: %v", err)
	}
}

func TestFileRecognizer_WaitForResult_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := DescribeTaskStatusResponse{}
		resp.Response.Data = &TaskStatus{
			RecTaskId: "task-slow",
			Status:    TaskStatusWaiting,
			StatusStr: "waiting",
		}
		resp.Response.RequestId = "req-slow"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	_, err := recognizer.WaitForResultWithInterval("task-slow", 20*time.Millisecond, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "not completed") {
		t.Errorf("expected timeout message, got: %v", err)
	}
}

func TestFileRecognizer_CreateTaskFromDataWithOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqBody CreateRecTaskRequest
		json.NewDecoder(r.Body).Decode(&reqBody)

		if reqBody.FilterDirty != 1 {
			t.Errorf("expected FilterDirty=1, got %d", reqBody.FilterDirty)
		}
		if reqBody.ResTextFormat != 2 {
			t.Errorf("expected ResTextFormat=2, got %d", reqBody.ResTextFormat)
		}
		if reqBody.HotwordId != "hw-123" {
			t.Errorf("expected HotwordId=hw-123, got %s", reqBody.HotwordId)
		}

		resp := CreateRecTaskResponse{}
		resp.Response.Data = &CreateRecTaskData{RecTaskId: "task-opts"}
		resp.Response.RequestId = "req-opts"
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cred := newTestCredential()
	recognizer := NewFileRecognizer(cred)
	recognizer.SetEndpoint(server.URL)

	req := &CreateRecTaskRequest{
		EngineModelType: "16k_zh_en",
		ChannelNum:      1,
		ResTextFormat:   2,
		FilterDirty:     1,
		HotwordId:       "hw-123",
	}

	taskID, err := recognizer.CreateTaskFromDataWithOptions([]byte("fake-audio"), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if taskID != "task-opts" {
		t.Errorf("unexpected task ID: %s", taskID)
	}
}

// TestSentenceWords_WireSpellings pins word-level offset parsing: the server
// returns StartTime / EndTime, while OffsetStartMs / OffsetEndMs is kept as a
// fallback. An explicit zero must not fall through to the fallback key.
func TestSentenceWords_WireSpellings(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		start int
		end   int
	}{
		{"server spelling", `{"Word":"今","StartTime":20,"EndTime":40}`, 20, 40},
		{"fallback spelling", `{"Word":"今","OffsetStartMs":120,"OffsetEndMs":640}`, 120, 640},
		{"explicit zero", `{"Word":"今","StartTime":0,"EndTime":160}`, 0, 160},
		{"missing offsets", `{"Word":"今"}`, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w SentenceWords
			if err := json.Unmarshal([]byte(tc.raw), &w); err != nil {
				t.Fatalf("Unmarshal failed: %v", err)
			}
			if w.Word != "今" {
				t.Errorf("Word = %q, want 今", w.Word)
			}
			if w.OffsetStartMs != tc.start || w.OffsetEndMs != tc.end {
				t.Errorf("offsets = %d/%d, want %d/%d",
					w.OffsetStartMs, w.OffsetEndMs, tc.start, tc.end)
			}
		})
	}
}
