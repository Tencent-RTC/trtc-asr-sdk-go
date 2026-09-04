// Package main demonstrates how to use the TRTC-ASR Go SDK for one-shot sentence recognition.
//
// Usage:
//
//	go run main.go -f test.wav
//	go run main.go -f test.pcm -e 16k_zh_en -fmt pcm
//	go run main.go -u https://example.com/test.wav -fmt wav
//
// Prerequisites:
//  1. Get Tencent Cloud APPID: https://console.cloud.tencent.com/cam/capi
//  2. Create a TRTC application: https://console.cloud.tencent.com/trtc/app
//  3. Get SDKAppID and SDK secret key from the application overview page
//  4. Prepare an audio file (duration <= 60s, size <= 3MB)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/asr"
	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// ===== Configuration =====
// Fill in your credentials before running.
var (
	AppID     = 0  // Tencent Cloud APPID (https://console.cloud.tencent.com/cam/capi)
	SdkAppID  = 0  // TRTC application ID (e.g., 1400188366)
	SecretKey = "" // TRTC SDK secret key
)

func main() {
	filePath := flag.String("f", "", "path to local audio file")
	audioURL := flag.String("u", "", "URL of audio file")
	engine := flag.String("e", "16k_zh_en", "engine model type (16k_zh, 16k_zh_en)")
	voiceFmt := flag.String("fmt", "pcm", "audio format (wav, pcm, ogg-opus, mp3, m4a)")
	wordInfo := flag.Int("w", 0, "word-level timing: 0=hide, 1=show, 2=show with punctuation")
	flag.Parse()

	if AppID == 0 || SdkAppID == 0 || SecretKey == "" {
		log.Fatal("Error: Please set AppID, SdkAppID and SecretKey in the code.\n\n" +
			"Steps:\n" +
			"  1. Get APPID from CAM Console: https://console.cloud.tencent.com/cam/capi\n" +
			"  2. Open TRTC Console: https://console.cloud.tencent.com/trtc/app\n" +
			"  3. Create or select an application\n" +
			"  4. Copy SDKAppID and SDK secret key from the application overview\n" +
			"  5. Fill in the credentials at the top of this file.\n")
	}

	if *filePath == "" && *audioURL == "" {
		log.Fatal("Error: Please specify either -f (local file) or -u (audio URL).\n\n" +
			"Examples:\n" +
			"  go run main.go -f test.pcm\n" +
			"  go run main.go -f test.wav -fmt wav\n" +
			"  go run main.go -u https://example.com/test.wav -fmt wav\n")
	}

	credential := common.NewCredential(AppID, SdkAppID, SecretKey)
	recognizer := asr.NewSentenceRecognizer(credential)

	var result *asr.SentenceRecognitionResult
	var err error

	if *audioURL != "" {
		// Recognize from URL
		log.Printf("Recognizing from URL: %s", *audioURL)
		result, err = recognizer.RecognizeURL(*audioURL, *voiceFmt, *engine)
	} else {
		// Recognize from local file
		data, readErr := os.ReadFile(*filePath)
		if readErr != nil {
			log.Fatalf("Failed to read audio file: %v", readErr)
		}
		log.Printf("Recognizing from file: %s (%d bytes)", *filePath, len(data))

		if *wordInfo > 0 {
			req := &asr.SentenceRecognitionRequest{
				EngServiceType: *engine,
				SourceType:     asr.SourceTypeData,
				VoiceFormat:    *voiceFmt,
				WordInfo:       *wordInfo,
			}
			result, err = recognizer.RecognizeDataWithOptions(data, req)
		} else {
			result, err = recognizer.RecognizeData(data, *voiceFmt, *engine)
		}
	}

	if err != nil {
		log.Fatalf("Recognition failed: %v", err)
	}

	fmt.Printf("Result: %s\n", result.Result)
	fmt.Printf("Audio Duration: %d ms\n", result.AudioDuration)
	fmt.Printf("Request ID: %s\n", result.RequestId)

	if len(result.WordList) > 0 {
		fmt.Printf("Word Count: %d\n", result.WordSize)
		for i, w := range result.WordList {
			fmt.Printf("  [%d] %s (%d-%d ms)\n", i, w.Word, w.StartTime, w.EndTime)
		}
	}
}
