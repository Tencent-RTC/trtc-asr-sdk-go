// Package main demonstrates how to use the TRTC-ASR Go SDK for audio file recognition.
//
// Unlike sentence recognition (≤60s), file recognition supports longer audio files
// via an async workflow: submit a task, then poll for results.
//
// Usage:
//
//	go run main.go -f ../test.pcm -fmt pcm
//	go run main.go -u https://example.com/test.wav
//	go run main.go -f audio.mp3 -fmt mp3 -e 16k_zh
//
// Prerequisites:
//  1. Get Tencent Cloud APPID: https://console.cloud.tencent.com/cam/capi
//  2. Create a TRTC application: https://console.cloud.tencent.com/trtc/app
//  3. Get SDKAppID and SDK secret key from the application overview page
//  4. Prepare an audio file (local ≤5MB, URL ≤1GB / ≤12h)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

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
	filePath := flag.String("f", "", "path to local audio file (≤5MB)")
	audioURL := flag.String("u", "", "URL of audio file (≤1GB, ≤12h)")
	engine := flag.String("e", "16k_zh_en", "engine model type (16k_zh, 16k_zh_en)")
	resFormat := flag.Int("res", 1, "result format: 0=basic, 1=detailed, 2=detailed with punctuation timing")
	callbackURL := flag.String("callback", "", "callback URL for receiving results when task completes")
	diarization := flag.Int("diarization", 0, "speaker diarization: 0=off, 1=cluster, 3=voiceprint roles")
	speakerNumber := flag.Int("speakers", 0, "expected speaker count hint (0=auto)")
	roleSpec := flag.String("roles", "", "voiceprint roles for -diarization=3: \"name=https://url,name2=https://url2\"")
	noiseThreshold := flag.Float64("noise-threshold", -1, "VAD noise threshold [0,4]; negative means unset")
	vadLevel := flag.Int("vad-level", -1, "VAD profile: 0=high recall, 1=far-field; negative means unset")
	pollInterval := flag.Duration("poll", time.Second, "poll interval for checking task status")
	maxWait := flag.Duration("timeout", 10*time.Minute, "max wait time for task completion")
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
			"  go run main.go -f ../test.pcm -fmt pcm\n" +
			"  go run main.go -u https://example.com/test.wav\n" +
			"  go run main.go -f audio.mp3 -fmt mp3 -e 16k_zh\n" +
			"  go run main.go -u https://example.com/call.wav -diarization 1\n")
	}

	credential := common.NewCredential(AppID, SdkAppID, SecretKey)
	recognizer := asr.NewFileRecognizer(credential)

	// applyOptions fills the shared recognition options on either request shape.
	applyOptions := func(req *asr.CreateRecTaskRequest) {
		req.SpeakerDiarization = *diarization
		req.SpeakerNumber = *speakerNumber
		req.SpeakerRoles = parseRoles(*roleSpec)
		if *vadLevel >= 0 {
			level := *vadLevel
			req.VadLevel = &level
		}
		if *noiseThreshold >= 0 {
			threshold := *noiseThreshold
			req.NoiseThreshold = &threshold
		}
	}

	var taskID string
	var err error

	if *audioURL != "" {
		log.Printf("Submitting URL task: %s", *audioURL)
		req := &asr.CreateRecTaskRequest{
			EngineModelType: *engine,
			ChannelNum:      1,
			ResTextFormat:   *resFormat,
			SourceType:      asr.SourceTypeURL,
			Url:             *audioURL,
			CallbackUrl:     *callbackURL,
		}
		applyOptions(req)
		taskID, err = recognizer.CreateTask(req)
	} else {
		data, readErr := os.ReadFile(*filePath)
		if readErr != nil {
			log.Fatalf("Failed to read audio file: %v", readErr)
		}
		log.Printf("Submitting file task: %s (%d bytes)", *filePath, len(data))

		req := &asr.CreateRecTaskRequest{
			EngineModelType: *engine,
			ChannelNum:      1,
			ResTextFormat:   *resFormat,
			CallbackUrl:     *callbackURL,
		}
		applyOptions(req)
		taskID, err = recognizer.CreateTaskFromDataWithOptions(data, req)
	}

	if err != nil {
		log.Fatalf("Failed to create task: %v", err)
	}

	fmt.Printf("Task created: %s\n", taskID)
	fmt.Printf("Polling for result (interval=%v, timeout=%v)...\n", *pollInterval, *maxWait)

	status, err := recognizer.WaitForResultWithInterval(taskID, *pollInterval, *maxWait)
	if err != nil {
		log.Fatalf("Task failed: %v", err)
	}

	fmt.Printf("\n=== Recognition Result ===\n")
	fmt.Printf("Task ID: %s\n", status.RecTaskId)
	fmt.Printf("Status: %s\n", status.StatusStr)
	fmt.Printf("Audio Duration: %.2f s\n", status.AudioDuration)
	fmt.Printf("Result: %s\n", status.Result)

	if len(status.ResultDetail) > 0 {
		fmt.Printf("\n=== Sentence Details ===\n")
		for i, detail := range status.ResultDetail {
			fmt.Printf("[%d] %s%s (%d-%d ms, speed=%.1f words/s)\n",
				i, speakerLabel(detail), detail.FinalSentence,
				detail.StartMs, detail.EndMs, detail.SpeechSpeed)

			if len(detail.Words) > 0 {
				for j, w := range detail.Words {
					fmt.Printf("    [%d] %s (%d-%d ms)\n",
						j, w.Word, w.OffsetStartMs, w.OffsetEndMs)
				}
			}
		}
	}
}

// speakerLabel renders the speaker prefix of a sentence: the enrolled role name
// wins, then the stereo channel, then the clustered speaker number.
func speakerLabel(detail asr.SentenceDetail) string {
	switch {
	case detail.SpeakerRoleName != "":
		return fmt.Sprintf("[%s] ", detail.SpeakerRoleName)
	case detail.ChannelId > 0:
		return fmt.Sprintf("[ch%d] ", detail.ChannelId)
	case detail.SpeakerId != 0:
		return fmt.Sprintf("[spk%d] ", detail.SpeakerId)
	default:
		return ""
	}
}

// parseRoles converts "name=url,name2=url2" into voiceprint enrollment roles.
func parseRoles(spec string) []asr.SpeakerRole {
	if spec == "" {
		return nil
	}
	var roles []asr.SpeakerRole
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, audioURL, ok := strings.Cut(entry, "=")
		if !ok {
			log.Fatalf("Invalid -roles entry %q, expected name=https://url", entry)
		}
		roles = append(roles, asr.SpeakerRole{
			RoleName: strings.TrimSpace(name),
			AudioUrl: strings.TrimSpace(audioURL),
		})
	}
	return roles
}
