package asr

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// assertSDKReportParams checks that a captured request query carries the SDK
// identification the service relies on for diagnostics.
func assertSDKReportParams(t *testing.T, query url.Values) {
	t.Helper()

	if got := query.Get("sdk_lang"); got != common.SDKLanguage {
		t.Errorf("sdk_lang = %q, want %q", got, common.SDKLanguage)
	}
	if got := query.Get("sdk_type"); got != common.SDKType {
		t.Errorf("sdk_type = %q, want %q", got, common.SDKType)
	}
	if got := query.Get("version"); got != common.SDKVersion {
		t.Errorf("version = %q, want %q", got, common.SDKVersion)
	}
	if got := query.Get("platform"); got != common.SDKPlatform() {
		t.Errorf("platform = %q, want %q", got, common.SDKPlatform())
	}
}

func TestSentenceRecognizer_ReportsSDKIdentity(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Response":{"RequestId":"req-1","Result":"hello"}}`))
	}))
	defer srv.Close()

	recognizer := NewSentenceRecognizer(newTestCredential())
	recognizer.SetEndpoint(srv.URL)

	if _, err := recognizer.Recognize(&SentenceRecognitionRequest{
		EngServiceType: "16k_zh",
		VoiceFormat:    "wav",
		SourceType:     SourceTypeURL,
		Url:            "https://example.com/test.wav",
	}); err != nil {
		t.Fatalf("Recognize() error = %v", err)
	}

	assertSDKReportParams(t, gotQuery)
	// The pre-existing protocol parameters must survive the addition.
	if gotQuery.Get("RequestId") == "" {
		t.Error("RequestId missing from query")
	}
}

func TestFileRecognizer_ReportsSDKIdentity(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Response":{"RequestId":"req-1","Data":{"RecTaskId":"task-42"}}}`))
	}))
	defer srv.Close()

	recognizer := NewFileRecognizer(newTestCredential())
	recognizer.SetEndpoint(srv.URL)

	if _, err := recognizer.CreateTask(&CreateRecTaskRequest{
		EngineModelType: "16k_zh",
		ChannelNum:      1,
		ResTextFormat:   0,
		SourceType:      SourceTypeURL,
		Url:             "https://example.com/test.wav",
	}); err != nil {
		t.Fatalf("CreateTask() error = %v", err)
	}

	assertSDKReportParams(t, gotQuery)
}

func TestSignatureParams_ReportsSDKIdentity(t *testing.T) {
	params := common.NewSignatureParams(12345, "16k_zh", "voice-1")
	query, err := url.ParseQuery(params.BuildQueryStringWithSignature("sig"))
	if err != nil {
		t.Fatalf("ParseQuery() error = %v", err)
	}

	assertSDKReportParams(t, query)
}
