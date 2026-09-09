// Sentence (one-shot) speech recognition example over the v3 protocol
// (POST /v3/transcribe).
//
// Recognizes a local audio file (<=60s, <=3MB).
//
// Credentials come from environment variables:
//
//	TRTC_ASR_SDK_APP_ID, TRTC_ASR_SECRET_KEY
//
// (v3 does not need the Tencent Cloud APPID.)
//
// Prerequisite: the server has enabled the EnableV3Route gray switch for
// your SDKAppID, otherwise requests fail with 404/4001.
//
// Usage: go run main.go -f ../test.pcm [engine]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	v3 "github.com/Tencent-RTC/trtc-asr-sdk-go/asr/v3"
)

func main() {
	file := flag.String("f", "../test.pcm", "audio file path (<=60s, <=3MB)")
	format := flag.String("format", "pcm", "audio format: wav|pcm|ogg-opus|mp3|m4a")
	wordInfo := flag.Int("word-info", 0, "word-level timestamps: 0=off, 1=on, 2=with punctuation")
	flag.Parse()

	sdkAppID, _ := strconv.Atoi(os.Getenv("TRTC_ASR_SDK_APP_ID"))
	secretKey := os.Getenv("TRTC_ASR_SECRET_KEY")
	if sdkAppID == 0 || secretKey == "" {
		log.Fatal("Set TRTC_ASR_SDK_APP_ID and TRTC_ASR_SECRET_KEY first.")
	}
	engine := "16k_zh_en"
	if flag.NArg() > 0 {
		engine = flag.Arg(0)
	}

	// v3 credentials need only SdkAppID + SecretKey (no Tencent Cloud APPID).
	credential := v3.NewCredential(sdkAppID, secretKey)
	recognizer := v3.NewSentenceRecognizer(credential)

	data, err := os.ReadFile(*file)
	if err != nil {
		log.Fatalf("read audio failed: %v", err)
	}

	var resp *v3.TranscribeResponse
	if *wordInfo != 0 {
		// With word_info: use the full request form.
		resp, err = recognizer.RecognizeDataWithOptions(data, &v3.TranscribeRequest{
			EngineModelType: engine,
			VoiceFormat:     *format,
			WordInfo:        *wordInfo,
		})
	} else {
		resp, err = recognizer.RecognizeData(data, *format, engine)
	}
	if err != nil {
		// Server codes (4xxx/5xxx) are carried on the error; 10xx codes are
		// SDK-local.
		log.Fatalf("recognize failed: %v", err)
	}

	fmt.Printf("Result: %s\n", resp.Result)
	fmt.Printf("Duration: %d ms  RequestId: %s\n", resp.AudioDuration, resp.RequestID)
	for _, w := range resp.WordList {
		fmt.Printf("  [%6d - %6d] %s\n", w.StartTime, w.EndTime, w.Word)
	}
}
