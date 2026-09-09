package v3

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// TestCreateTaskWire verifies the /v3/create_transcription request shape and
// the transcription_id extraction.
func TestCreateTaskWire(t *testing.T) {
	srv, captured := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "req-1",
			"transcription_id": "tid-abc",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	id, err := r.CreateTask(&CreateTranscriptionRequest{
		EngineModelType:    "16k_zh_en",
		ChannelNum:         1,
		ResTextFormat:      1,
		SourceType:         SourceTypeURL,
		URL:                "https://example.com/a.wav",
		CallbackURL:        "https://example.com/cb",
		HotwordID:          "hw-1",
		VadSilenceMs:       600,
		VadLevel:           1,
		Language:           "zh",
		SpeakerDiarization: SpeakerDiarizationVoiceprint,
		SpeakerRoles: []SpeakerRole{
			{RoleName: "teacher", AudioURL: "https://example.com/t.wav"},
		},
		VoiceprintIDs: []string{"vp-1"},
	})
	if err != nil {
		t.Fatalf("CreateTask failed: %v", err)
	}
	if id != "tid-abc" {
		t.Errorf("transcription id = %q", id)
	}

	if captured.path != "/v3/create_transcription" {
		t.Errorf("path = %q", captured.path)
	}
	if captured.query != "" {
		t.Errorf("query must be empty, got %q", captured.query)
	}

	auth, params := parseEnvelope(t, captured.body)
	if auth["sdkappid"] != "1400000000" || auth["usersig"] == "" || auth["request_id"] == "" {
		t.Errorf("auth = %v", auth)
	}
	for _, k := range []string{
		"engine_model_type", "channel_num", "res_text_format", "source_type",
		"url", "callback_url", "hotword_id", "vad_silence_ms", "vad_level",
		"language", "speaker_diarization", "speaker_roles", "voiceprint_ids",
		"sdk_info",
	} {
		if _, ok := params[k]; !ok {
			t.Errorf("params.%s missing", k)
		}
	}
	// snake_case everywhere.
	for _, bad := range []string{"EngineModelType", "ChannelNum", "ResTextFormat", "SourceType", "Url", "CallbackUrl", "HotwordId"} {
		if _, ok := params[bad]; ok {
			t.Errorf("params must be snake_case, found %s", bad)
		}
	}
	// speaker_roles elements are snake_case.
	roles := params["speaker_roles"].([]interface{})
	role := roles[0].(map[string]interface{})
	if role["role_name"] != "teacher" || role["audio_url"] != "https://example.com/t.wav" {
		t.Errorf("speaker_roles[0] = %v", role)
	}
}

// TestCreateTaskError verifies business errors pass through with the server
// code (here 4006 concurrency limit, HTTP 429).
func TestCreateTaskError(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusTooManyRequests, map[string]interface{}{
			"code": 4006, "message": "concurrency limit", "request_id": "req-c",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.CreateTaskFromURL("https://example.com/a.wav", "16k_zh_en")
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != 4006 {
		t.Fatalf("error = %v, want ASRError code 4006", err)
	}
}

// TestCreateTaskMissingID: code=0 without transcription_id is a protocol
// violation — the SDK must fail rather than hand back an unusable ID.
func TestCreateTaskMissingID(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "req-1",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	if _, err := r.CreateTaskFromURL("https://example.com/a.wav", "16k_zh_en"); err == nil {
		t.Fatal("empty transcription_id must fail")
	}
}

// TestDescribeTaskPoll drives WaitForResult through waiting → success and
// verifies the result_detail mapping (snake_case words, speaker fields).
func TestDescribeTaskPoll(t *testing.T) {
	var calls int32
	srv, captured := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
				"code": 0, "message": "success", "request_id": "r1",
				"transcription_id": "tid-abc", "status": 1, "status_str": "executing",
				"progress": 40,
			})
			return
		}
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "r2",
			"transcription_id": "tid-abc", "status": 2, "status_str": "success",
			"progress": 100, "audio_duration": 6.312, "result": "全文",
			"result_detail": []map[string]interface{}{
				{
					"final_sentence": "第一句话。", "start_ms": 0, "end_ms": 1200,
					"words_num":  1,
					"words":      []map[string]interface{}{{"word": "第一句", "start_time": 0, "end_time": 900}},
					"speaker_id": 1, "speaker_role_name": "teacher", "channel_id": 0,
					"speech_speed": 3.2, "language": "zh", "language_b47": "zh-CN",
				},
			},
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	status, err := r.WaitForResultWithInterval("tid-abc", 10*time.Millisecond, 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForResult failed: %v", err)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Errorf("calls = %d, want >= 2 (poll until success)", calls)
	}
	if captured.path != "/v3/describe_transcription" {
		t.Errorf("path = %q", captured.path)
	}
	_, params := parseEnvelope(t, captured.body)
	if params["transcription_id"] != "tid-abc" {
		t.Errorf("params.transcription_id = %v", params["transcription_id"])
	}

	if status.Status != TaskStatusSuccess || status.Result != "全文" || status.AudioDuration != 6.312 {
		t.Errorf("status = %+v", status)
	}
	if len(status.ResultDetail) != 1 {
		t.Fatalf("result_detail = %+v", status.ResultDetail)
	}
	d := status.ResultDetail[0]
	if d.FinalSentence != "第一句话。" || d.StartMs != 0 || d.EndMs != 1200 {
		t.Errorf("detail = %+v", d)
	}
	if d.SpeakerID != 1 || d.SpeakerRoleName != "teacher" {
		t.Errorf("speaker fields = %+v", d)
	}
	if d.Language != "zh" || d.LanguageB47 != "zh-CN" {
		t.Errorf("language fields = %+v", d)
	}
	if len(d.Words) != 1 || d.Words[0].StartTime != 0 || d.Words[0].EndTime != 900 {
		t.Errorf("words = %+v", d.Words)
	}
}

// TestDescribeTaskOwnership verifies a cross-account query error surfaces the
// 4002 code even though it arrives with HTTP 403.
func TestDescribeTaskOwnership(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusForbidden, map[string]interface{}{
			"code": 4002, "message": "transcription_id does not belong to this sdkappid",
			"request_id": "r1",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.DescribeTask("tid-foreign")
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != 4002 {
		t.Fatalf("error = %v, want ASRError code 4002", err)
	}
}

// TestWaitForResultFailed verifies a failed task surfaces error_msg.
func TestWaitForResultFailed(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "r1",
			"transcription_id": "tid-abc", "status": 3, "status_str": "failed",
			"error_msg": "audio decode failed",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.WaitForResult("tid-abc")
	if err == nil {
		t.Fatal("failed task must return error")
	}
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) {
		t.Fatalf("error type = %T", err)
	}
	if asrErr.Code != common.ErrCodeServerError {
		t.Errorf("code = %d, want ErrCodeServerError", asrErr.Code)
	}
}

// TestWaitForResultTimeout verifies the timeout path reports the last status.
func TestWaitForResultTimeout(t *testing.T) {
	srv, _ := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "r1",
			"transcription_id": "tid-abc", "status": 1, "status_str": "executing",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	_, err := r.WaitForResultWithInterval("tid-abc", 10*time.Millisecond, 50*time.Millisecond)
	var asrErr *common.ASRError
	if !errors.As(err, &asrErr) || asrErr.Code != common.ErrCodeTimeout {
		t.Fatalf("error = %v, want ErrCodeTimeout", err)
	}
}

// TestCreateTaskValidation covers local parameter validation.
func TestCreateTaskValidation(t *testing.T) {
	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint("http://127.0.0.1:0") // unreachable; local validation must fail first

	cases := []struct {
		name string
		req  *CreateTranscriptionRequest
	}{
		{"nil", nil},
		{"missing engine", &CreateTranscriptionRequest{ChannelNum: 1, SourceType: SourceTypeURL, URL: "https://a.wav"}},
		{"bad channel", &CreateTranscriptionRequest{EngineModelType: "16k_zh", ChannelNum: 3, SourceType: SourceTypeURL, URL: "https://a.wav"}},
		{"stereo with diarization", &CreateTranscriptionRequest{
			EngineModelType: "8k_zh", ChannelNum: 2, SourceType: SourceTypeURL, URL: "https://a.wav",
			SpeakerDiarization: SpeakerDiarizationCluster,
		}},
		{"url missing", &CreateTranscriptionRequest{EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL}},
		{"role without name", &CreateTranscriptionRequest{
			EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL, URL: "https://a.wav",
			SpeakerDiarization: SpeakerDiarizationVoiceprint,
			SpeakerRoles:       []SpeakerRole{{AudioURL: "https://a.wav"}},
		}},
		{"role bad url", &CreateTranscriptionRequest{
			EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL, URL: "https://a.wav",
			SpeakerDiarization: SpeakerDiarizationVoiceprint,
			SpeakerRoles:       []SpeakerRole{{RoleName: "t", AudioURL: "ftp://a.wav"}},
		}},
		{"roles need mode 3", &CreateTranscriptionRequest{
			EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL, URL: "https://a.wav",
			SpeakerDiarization: SpeakerDiarizationCluster,
			SpeakerRoles:       []SpeakerRole{{RoleName: "t", AudioURL: "https://a.wav"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.CreateTask(tc.req); err == nil {
				t.Error("must fail local validation")
			}
		})
	}
}

// TestVadValidation covers noise threshold and vad level ranges.
func TestVadValidation(t *testing.T) {
	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint("http://127.0.0.1:0")

	bad := 4.5
	_, err := r.CreateTask(&CreateTranscriptionRequest{
		EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL,
		URL: "https://a.wav", NoiseThreshold: &bad,
	})
	if err == nil {
		t.Error("noise_threshold > 4 must fail locally")
	}

	_, err = r.CreateTask(&CreateTranscriptionRequest{
		EngineModelType: "16k_zh", ChannelNum: 1, SourceType: SourceTypeURL,
		URL: "https://a.wav", VadLevel: 2,
	})
	if err == nil {
		t.Error("vad_level=2 must fail locally")
	}
}

// TestEmptyAndOversizedData covers the convenience methods' size guards.
func TestEmptyAndOversizedData(t *testing.T) {
	r := NewFileRecognizer(newTestCredential())
	if _, err := r.CreateTaskFromData(nil, "pcm", "16k_zh_en"); err == nil {
		t.Error("empty data must fail")
	}
	if _, err := r.CreateTaskFromData(make([]byte, 5*1024*1024+1), "pcm", "16k_zh_en"); err == nil {
		t.Error("oversized data must fail")
	}
}

// TestCreateTaskAudioURLs covers the distributed recording path: AudioURLs
// requires SourceType=0 with URL/Data left empty, and must reach the wire
// (earlier SDK validation rejected exactly this combination).
func TestCreateTaskAudioURLs(t *testing.T) {
	srv, captured := startHTTPServer(t, func(w http.ResponseWriter, c *capturedRequest) {
		writeFlatJSON(t, w, http.StatusOK, map[string]interface{}{
			"code": 0, "message": "success", "request_id": "req-d",
			"transcription_id": "tid-dist",
		})
	})

	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint(srv.URL)

	id, err := r.CreateTask(&CreateTranscriptionRequest{
		EngineModelType:    "16k_zh_en",
		ChannelNum:         1,
		ResTextFormat:      1,
		SourceType:         SourceTypeURL, // zero value; URL/Data stay empty
		SpeakerDiarization: SpeakerDiarizationCluster,
		AudioURLs: []AudioURLItem{
			{Index: 0, URL: "https://example.com/a.wav", Label: "a"},
			{Index: 1, URL: "https://example.com/b.wav"},
		},
	})
	if err != nil {
		t.Fatalf("AudioURLs task must pass local validation: %v", err)
	}
	if id != "tid-dist" {
		t.Errorf("transcription id = %q", id)
	}

	_, params := parseEnvelope(t, captured.body)
	urls, ok := params["audio_urls"].([]interface{})
	if !ok || len(urls) != 2 {
		t.Fatalf("params.audio_urls = %v", params["audio_urls"])
	}
	first := urls[0].(map[string]interface{})
	if first["index"] != float64(0) || first["url"] != "https://example.com/a.wav" || first["label"] != "a" {
		t.Errorf("audio_urls[0] = %v", first)
	}
	if _, ok := params["url"]; ok {
		t.Error("params.url must be omitted in distributed mode")
	}
}

// TestCreateTaskAudioURLsReject pins the server-side invariants locally.
func TestCreateTaskAudioURLsReject(t *testing.T) {
	r := NewFileRecognizer(newTestCredential())
	r.SetEndpoint("http://127.0.0.1:0") // unreachable; local validation must fail first

	base := func() *CreateTranscriptionRequest {
		return &CreateTranscriptionRequest{
			EngineModelType: "16k_zh_en", ChannelNum: 1, ResTextFormat: 1,
			AudioURLs: []AudioURLItem{{Index: 0, URL: "https://example.com/a.wav"}},
		}
	}

	cases := []struct {
		name   string
		mutate func(req *CreateTranscriptionRequest)
	}{
		{"with URL", func(req *CreateTranscriptionRequest) { req.URL = "https://example.com/x.wav" }},
		{"with Data", func(req *CreateTranscriptionRequest) { req.Data = "AAAA"; req.DataLen = 3 }},
		{"non-zero source_type", func(req *CreateTranscriptionRequest) { req.SourceType = SourceTypeData }},
		{"negative index", func(req *CreateTranscriptionRequest) { req.AudioURLs[0].Index = -1 }},
		{"duplicate index", func(req *CreateTranscriptionRequest) {
			req.AudioURLs = append(req.AudioURLs, AudioURLItem{Index: 0, URL: "https://example.com/b.wav"})
		}},
		{"empty url", func(req *CreateTranscriptionRequest) { req.AudioURLs[0].URL = " " }},
		{"non-http url", func(req *CreateTranscriptionRequest) { req.AudioURLs[0].URL = "ftp://x/a.wav" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(req)
			if _, err := r.CreateTask(req); err == nil {
				t.Error("must fail local validation")
			}
		})
	}
}

// TestWithOptionsNilRequest covers the nil-request guard of the convenience
// methods (they mutate the request in place).
func TestWithOptionsNilRequest(t *testing.T) {
	r := NewFileRecognizer(newTestCredential())
	if _, err := r.CreateTaskFromDataWithOptions([]byte("abc"), nil); err == nil {
		t.Error("nil request must fail with an error, not panic")
	}
}

// TestSentenceDetailUnmarshal ensures describe responses decode into the flat
// v3 shapes (guards against tag drift).
func TestSentenceDetailUnmarshal(t *testing.T) {
	raw := `{"code":0,"message":"success","request_id":"r","transcription_id":"t",
		"status":2,"status_str":"success","result_detail":[{
			"final_sentence":"a","slice_sentence":"b","written_text":"c",
			"start_ms":1,"end_ms":2,"words_num":1,
			"words":[{"word":"w","start_time":3,"end_time":4}],
			"speech_speed":1.5,"speaker_id":1,"channel_id":2,
			"speaker_role_name":"role",
			"silence_time":9,"language":"zh","language_b47":"zh-CN"}]}`
	var resp TranscriptionStatus
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	d := resp.ResultDetail[0]
	if d.FinalSentence != "a" || d.SliceSentence != "b" || d.WrittenText != "c" ||
		d.StartMs != 1 || d.EndMs != 2 || d.WordsNum != 1 ||
		d.Words[0].Word != "w" || d.Words[0].StartTime != 3 || d.Words[0].EndTime != 4 ||
		d.SpeechSpeed != 1.5 || d.SpeakerID != 1 || d.ChannelID != 2 ||
		d.SpeakerRoleName != "role" ||
		d.SilenceTime != 9 || d.Language != "zh" || d.LanguageB47 != "zh-CN" {
		t.Errorf("detail = %+v", d)
	}
}
