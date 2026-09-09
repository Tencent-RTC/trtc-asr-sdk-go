package v3

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// capturedRequest records what the mock server received.
type capturedRequest struct {
	path        string
	query       string
	contentType string
	body        []byte
}

// startHTTPServer starts a mock v3 HTTP endpoint. handler inspects the
// captured request and writes the response.
func startHTTPServer(t *testing.T, handler func(w http.ResponseWriter, captured *capturedRequest)) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.path = r.URL.Path
		captured.query = r.URL.RawQuery
		captured.contentType = r.Header.Get("Content-Type")
		captured.body = body
		handler(w, captured)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func writeFlatJSON(t *testing.T, w http.ResponseWriter, status int, v interface{}) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal response failed: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// parseEnvelope splits the captured body into auth and params.
func parseEnvelope(t *testing.T, body []byte) (auth, params map[string]interface{}) {
	t.Helper()
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	if err := json.Unmarshal(env["auth"], &auth); err != nil {
		t.Fatalf("auth block missing/invalid: %v", err)
	}
	if err := json.Unmarshal(env["params"], &params); err != nil {
		t.Fatalf("params block missing/invalid: %v", err)
	}
	return auth, params
}

func newTestCredential() *common.Credential {
	return NewCredential(1400000000, "test-secret")
}

// TestTranscribeWire verifies the request shape: POST /v3/transcribe, no query,
// no custom auth headers, {"auth":...,"params":...} in snake_case.
func TestTranscribeWire(t *testing.T) {
	srv, captured := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "req-1",
			"result": "你好世界", "audio_duration": 1500, "language": "zh",
			"word_size": 2,
			"word_list": []map[string]interface{}{
				{"word": "你好", "start_time": 0, "end_time": 500},
				{"word": "世界", "start_time": 500, "end_time": 1500},
			},
		})
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	needvad := 0
	resp, err := r.Recognize(&TranscribeRequest{
		EngineModelType: "16k_zh_en",
		SourceType:      SourceTypeData,
		VoiceFormat:     "pcm",
		Data:            base64.StdEncoding.EncodeToString([]byte("pcm-data")),
		DataLen:         8,
		HotwordList:     "腾讯云|10",
		Needvad:         &needvad,
		Language:        "zh",
		ConvertNumMode:  1,
		WordInfo:        1,
		FilterPunc:      1,
		InputSampleRate: 8000,
		CustomizationID: "cust-1",
		Context:         &Context{Text: "bg", Terms: []string{"ASR"}},
	})
	if err != nil {
		t.Fatalf("Recognize failed: %v", err)
	}

	// Transport shape.
	if captured.path != "/v3/transcribe" {
		t.Errorf("path = %q, want /v3/transcribe", captured.path)
	}
	if captured.query != "" {
		t.Errorf("query must be empty (v3 carries everything in body), got %q", captured.query)
	}
	if !strings.HasPrefix(captured.contentType, "application/json") {
		t.Errorf("content-type = %q", captured.contentType)
	}

	auth, params := parseEnvelope(t, captured.body)
	if auth["sdkappid"] != "1400000000" {
		t.Errorf("auth.sdkappid = %v", auth["sdkappid"])
	}
	if auth["usersig"] == "" {
		t.Error("auth.usersig is empty")
	}
	if auth["request_id"] == "" {
		t.Error("auth.request_id is empty")
	}

	// snake_case wire keys (v1 PascalCase names must not appear).
	want := map[string]interface{}{
		"engine_model_type": "16k_zh_en",
		"source_type":       float64(1),
		"voice_format":      "pcm",
		"data_len":          float64(8),
		"hotword_list":      "腾讯云|10",
		"needvad":           float64(0), // explicit 0 honored
		"language":          "zh",
		"convert_num_mode":  float64(1),
		"word_info":         float64(1),
		"filter_punc":       float64(1),
		"input_sample_rate": float64(8000),
		"customization_id":  "cust-1",
	}
	for k := range want {
		if _, ok := params[k]; !ok {
			t.Errorf("params.%s missing", k)
		}
	}
	for _, bad := range []string{"EngSerViceType", "SourceType", "VoiceFormat", "Data", "Url", "DataLen", "HotwordList"} {
		if _, ok := params[bad]; ok {
			t.Errorf("params must be snake_case, found %s", bad)
		}
	}
	if _, ok := params["sdk_info"].(map[string]interface{}); !ok {
		t.Error("params.sdk_info missing")
	}
	if _, ok := params["context"].(map[string]interface{}); !ok {
		t.Error("params.context missing")
	}
	if params["data"] != base64.StdEncoding.EncodeToString([]byte("pcm-data")) {
		t.Errorf("params.data = %v", params["data"])
	}

	// Response mapping.
	if resp.Result != "你好世界" || resp.AudioDuration != 1500 || resp.Language != "zh" {
		t.Errorf("response = %+v", resp)
	}
	if resp.RequestID != "req-1" {
		t.Errorf("request_id = %q", resp.RequestID)
	}
	if resp.WordSize != 2 || len(resp.WordList) != 2 {
		t.Fatalf("word list = %+v", resp.WordList)
	}
	if resp.WordList[0].Word != "你好" || resp.WordList[0].StartTime != 0 || resp.WordList[0].EndTime != 500 {
		t.Errorf("word[0] = %+v", resp.WordList[0])
	}
}

// TestTranscribeAuthErrorHTTP200 locks the v3 quirk: auth failure arrives with
// HTTP 200 and code 4002 — the SDK must judge by body code, not HTTP status.
func TestTranscribeAuthErrorHTTP200(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 4002, "message": "auth failed", "request_id": "req-x",
		})
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.RecognizeURL("https://example.com/a.wav", "wav", "16k_zh_en")
	if err == nil {
		t.Fatal("must fail on code 4002")
	}
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != 4002 {
		t.Fatalf("error = %v, want ASRError code 4002", err)
	}
	if !strings.Contains(asrErr.Message, "req-x") {
		t.Errorf("error message should carry request_id: %v", asrErr.Message)
	}
}

// TestTranscribeHTTPError verifies gateway-style failures (non-200 + flat
// error body) surface the body code.
func TestTranscribeHTTPError(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusServiceUnavailable, map[string]interface{}{
			"code": 5000, "message": "no worker available", "request_id": "req-y",
		})
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.RecognizeURL("https://example.com/a.wav", "wav", "16k_zh_en")
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != 5000 {
		t.Fatalf("error = %v, want ASRError code 5000", err)
	}
}

// TestTranscribeInvalidBody covers a non-JSON error body (e.g. an LB error
// page): the SDK must report an error including the HTTP status.
func TestTranscribeInvalidBody(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.RecognizeURL("https://example.com/a.wav", "wav", "16k_zh_en")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %v, want mention of http 502", err)
	}
}

// TestTranscribeConvenience verifies the convenience constructors populate
// source_type/data/url correctly.
func TestTranscribeConvenience(t *testing.T) {
	srv, captured := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "r1", "result": "ok",
		})
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	if _, err := r.RecognizeData([]byte("abc"), "pcm", "16k_zh_en"); err != nil {
		t.Fatalf("RecognizeData failed: %v", err)
	}
	_, params := parseEnvelope(t, captured.body)
	if params["source_type"] != float64(1) || params["data"] != base64.StdEncoding.EncodeToString([]byte("abc")) || params["data_len"] != float64(3) {
		t.Errorf("data params = %v", params)
	}

	if _, err := r.RecognizeURL("https://example.com/a.wav", "wav", "16k_zh_en"); err != nil {
		t.Fatalf("RecognizeURL failed: %v", err)
	}
	_, params = parseEnvelope(t, captured.body)
	if params["source_type"] != float64(0) || params["url"] != "https://example.com/a.wav" {
		t.Errorf("url params = %v", params)
	}

	// Local validation.
	if _, err := r.RecognizeData(nil, "pcm", "16k_zh_en"); err == nil {
		t.Error("empty data must fail locally")
	}
	if _, err := r.Recognize(&TranscribeRequest{SourceType: SourceTypeData, VoiceFormat: "pcm"}); err == nil {
		t.Error("missing EngineModelType must fail locally")
	}
	// nil request must return an error, not panic.
	if _, err := r.RecognizeDataWithOptions([]byte("abc"), nil); err == nil {
		t.Error("nil request must fail with an error, not panic")
	}
}

// TestTranscribeNonV3JSONBody covers the gateway/LB failure shape: a non-2xx
// response with a JSON body that is not a v3 response (no numeric code) must
// not be mistaken for success.
func TestTranscribeNonV3JSONBody(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
	})

	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.RecognizeURL("https://example.com/a.wav", "wav", "16k_zh_en")
	if err == nil {
		t.Fatal("non-2xx JSON body without code must not be treated as success")
	}
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != common.ErrCodeServerError {
		t.Fatalf("error = %v, want ErrCodeServerError", err)
	}
	if !strings.Contains(asrErr.Message, "502") {
		t.Errorf("error should carry the http status: %v", asrErr.Message)
	}
}

// TestTranscribeLocalRanges pins the local ranges added for parity with the
// online validator: needvad (0/1) and input_sample_rate (0/8000).
func TestTranscribeLocalRanges(t *testing.T) {
	r := NewSentenceRecognizer(newTestCredential())
	r.SetEndpoint("http://127.0.0.1:0") // unreachable; validation must fail first

	badNeedvad := 2
	if _, err := r.Recognize(&TranscribeRequest{
		EngineModelType: "16k_zh_en", SourceType: SourceTypeURL,
		VoiceFormat: "wav", URL: "https://example.com/a.wav", Needvad: &badNeedvad,
	}); err == nil {
		t.Error("needvad=2 must fail locally")
	}
	if _, err := r.Recognize(&TranscribeRequest{
		EngineModelType: "16k_zh_en", SourceType: SourceTypeURL,
		VoiceFormat: "wav", URL: "https://example.com/a.wav", InputSampleRate: 16000,
	}); err == nil {
		t.Error("input_sample_rate=16000 must fail locally")
	}
}
