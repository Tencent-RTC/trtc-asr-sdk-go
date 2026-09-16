// Async audio file recognition example over the v3 protocol
// (POST /v3/create_transcription + /v3/describe_transcription).
//
// Credentials come from environment variables:
//
//	TRTC_ASR_SDK_APP_ID, TRTC_ASR_SECRET_KEY
//
// (v3 does not need the Tencent Cloud APPID.)
//
// Usage: go run main.go [-f local.wav | -u https://example.com/audio.wav] [engine]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	v3 "github.com/Tencent-RTC/trtc-asr-sdk-go/asr/v3"
)

func main() {
	file := flag.String("f", "", "local audio file (<=5MB)")
	url := flag.String("u", "", "audio URL (<=12h, <=1GB)")
	diarization := flag.Int("diarization", 0, "speaker diarization: 0=off, 1=cluster, 3=voiceprint roles")
	lang := flag.String("lang", "", "language hint; when omitted, the bigmodel engine uses zh")
	interval := flag.Duration("interval", time.Second, "poll interval")
	timeout := flag.Duration("timeout", 10*time.Minute, "poll timeout")
	flag.Parse()

	sdkAppID, _ := strconv.Atoi(os.Getenv("TRTC_ASR_SDK_APP_ID"))
	secretKey := os.Getenv("TRTC_ASR_SECRET_KEY")
	if sdkAppID == 0 || secretKey == "" {
		log.Fatal("Set TRTC_ASR_SDK_APP_ID and TRTC_ASR_SECRET_KEY first.")
	}
	engine := ""
	if flag.NArg() > 0 {
		engine = flag.Arg(0)
	}
	if engine == "" {
		fmt.Fprintln(os.Stderr, "error: engine argument is required (e.g. bigmodel)")
		flag.Usage()
		os.Exit(2)
	}
	// The bigmodel engine is best used with an explicit language; every other
	// engine falls back to server-side detection unless -lang is given.
	if *lang == "" && engine == "bigmodel" {
		*lang = "zh"
	}
	if (*file == "") == (*url == "") {
		log.Fatal("Pass exactly one of -f (local file) or -u (URL).")
	}

	// v3 credentials need only SdkAppID + SecretKey (no Tencent Cloud APPID).
	credential := v3.NewCredential(sdkAppID, secretKey)
	recognizer := v3.NewFileRecognizer(credential)

	var taskID string
	var err error
	req := &v3.CreateTranscriptionRequest{
		EngineModelType:    engine,
		ChannelNum:         1,
		ResTextFormat:      1, // include word-level timestamps
		SpeakerDiarization: *diarization,
		Language:           *lang,
	}
	if *url != "" {
		req.SourceType = v3.SourceTypeURL
		req.URL = *url
		taskID, err = recognizer.CreateTask(req)
	} else {
		var data []byte
		data, err = os.ReadFile(*file)
		if err == nil {
			taskID, err = recognizer.CreateTaskFromDataWithOptions(data, req)
		}
	}
	if err != nil {
		log.Fatalf("create task failed: %v", err)
	}
	fmt.Printf("Task created: %s\n", taskID)

	status, err := recognizer.WaitForResultWithInterval(taskID, *interval, *timeout)
	if err != nil {
		log.Fatalf("wait result failed: %v", err)
	}

	fmt.Printf("Status: %s  Duration: %.2f s\n", status.StatusStr, status.AudioDuration)
	fmt.Printf("Result: %s\n", status.Result)
	for _, d := range status.ResultDetail {
		speaker := ""
		if d.SpeakerRoleName != "" {
			speaker = " [" + d.SpeakerRoleName + "]"
		} else if d.SpeakerID > 0 {
			speaker = fmt.Sprintf(" [spk%d]", d.SpeakerID)
		}
		fmt.Printf("  [%6d - %6d]%s %s\n", d.StartMs, d.EndMs, speaker, d.FinalSentence)
	}
}
