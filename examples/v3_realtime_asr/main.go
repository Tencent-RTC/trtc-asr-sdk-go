// Package main demonstrates the TRTC-ASR v3 protocol client (package asr/v3):
// the WebSocket URL carries only voice_id, and auth/params travel in the
// start frame.
//
// Usage:
//
//	go run main.go -f ../test.pcm
//	go run main.go -f ../test.pcm -e bigmodel -lang zh -diarization 1 -word-info 1
//
// Prerequisites:
//  1. Create a TRTC application: https://console.cloud.tencent.com/trtc/app
//     (SDKAppID + SDK secret key from the application overview page; v3 does
//     not need the Tencent Cloud APPID)
//  2. Prepare a PCM audio file (16kHz, 16bit, mono)
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	v3 "github.com/Tencent-RTC/trtc-asr-sdk-go/asr/v3"
)

// ===== Configuration =====
// Fill in your credentials before running, or export the env vars.
var (
	SdkAppID  = 0  // TRTC application ID (e.g., 1400188366)
	SecretKey = "" // TRTC SDK secret key
)

const (
	envSdkAppID  = "TRTC_ASR_SDK_APP_ID"
	envSecretKey = "TRTC_ASR_SECRET_KEY"
)

// MyListener implements the v3 SpeechRecognitionListener interface.
type MyListener struct {
	v3.UnimplementedSpeechRecognitionListener
}

func (l *MyListener) OnRecognitionStart(resp *v3.SpeechRecognitionResponse) {
	log.Printf("Recognition started, voice_id: %s", resp.VoiceID)
}

func (l *MyListener) OnSentenceBegin(resp *v3.SpeechRecognitionResponse) {
	log.Printf("Sentence begin, index: %d", resp.Result.Index)
}

func (l *MyListener) OnRecognitionResultChange(resp *v3.SpeechRecognitionResponse) {
	log.Printf("Result change, index: %d, text: %s", resp.Result.Index, resp.Result.VoiceTextStr)
}

func (l *MyListener) OnSentenceEnd(resp *v3.SpeechRecognitionResponse) {
	log.Printf("Sentence end, index: %d, text: %s", resp.Result.Index, resp.Result.VoiceTextStr)
	for _, seg := range resp.Result.SpeakerSegments {
		label := seg.SpeakerName // only returned with diarization mode 3
		if label == "" {
			label = fmt.Sprintf("spk%d", seg.SpeakerID)
		}
		log.Printf("  [%s] %s (%d-%d ms)", label, seg.Text, seg.StartTime, seg.EndTime)
	}
}

func (l *MyListener) OnRecognitionComplete(resp *v3.SpeechRecognitionResponse) {
	log.Printf("Recognition complete, voice_id: %s", resp.VoiceID)
}

func (l *MyListener) OnFail(resp *v3.SpeechRecognitionResponse, err error) {
	log.Printf("Recognition failed: %v", err)
}

func main() {
	filePath := flag.String("f", "../test.pcm", "path to audio file (PCM)")
	engine := flag.String("e", "", "engine model type, required (e.g. bigmodel)")
	lang := flag.String("lang", "", "language hint; when omitted, the bigmodel engine uses zh")
	diarization := flag.Int("diarization", 0, "speaker diarization: 0=off, 1=cluster, 3=voiceprint roles")
	roleSpec := flag.String("roles", "", "voiceprint roles for -diarization=3: \"name=https://url,name2=https://url2\"")
	wordInfo := flag.Int("word-info", 0, "word-level timestamps: 0=off, 1=on, 2=with punctuation")
	hotwordList := flag.String("hotwords", "", "temporary hotword list: \"word|weight,word|weight\"")
	flag.Parse()

	if *engine == "" {
		fmt.Fprintln(os.Stderr, "error: -e is required (engine model type, e.g. -e bigmodel)")
		flag.Usage()
		os.Exit(2)
	}

	// The bigmodel engine is best used with an explicit language; every other
	// engine falls back to server-side detection unless -lang is given.
	if *lang == "" && *engine == "bigmodel" {
		*lang = "zh"
	}

	loadCredentialsFromEnv()
	if SdkAppID == 0 || SecretKey == "" {
		log.Fatal("Error: Please set SdkAppID and SecretKey in the code or export " +
			"TRTC_ASR_SDK_APP_ID and TRTC_ASR_SECRET_KEY.")
	}
	if _, err := os.Stat(*filePath); os.IsNotExist(err) {
		log.Fatalf("Error: Audio file not found: %s", *filePath)
	}

	// v3 credentials need only SdkAppID + SecretKey (no Tencent Cloud APPID).
	credential := v3.NewCredential(SdkAppID, SecretKey)

	recognizer := v3.NewSpeechRecognizer(credential, *engine, &MyListener{})
	if *lang != "" {
		recognizer.SetLanguage(*lang)
	}
	if *wordInfo != 0 {
		recognizer.SetWordInfo(*wordInfo)
	}
	if *hotwordList != "" {
		recognizer.SetHotwordList(*hotwordList)
	}
	if *diarization != 0 {
		recognizer.SetSpeakerDiarization(*diarization)
		if roles := parseRoles(*roleSpec); len(roles) > 0 {
			recognizer.SetSpeakerRoles(roles)
		}
	}

	// v3 Start waits for the server ack: auth/params errors (4001/4002/...)
	// surface here synchronously.
	if err := recognizer.Start(); err != nil {
		log.Fatalf("Failed to start recognizer: %v", err)
	}

	file, err := os.Open(*filePath)
	if err != nil {
		log.Fatalf("Failed to open file: %v", err)
	}
	defer file.Close()

	// 200ms of 16kHz 16bit mono PCM. The server rate-limits to at most 3s of
	// audio per 1s wall-clock (error 4000): keep the pacing when enlarging
	// the buffer.
	buf := make([]byte, 6400)
	for {
		n, err := file.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("Failed to read audio file: %v", err)
			break
		}
		if writeErr := recognizer.Write(buf[:n]); writeErr != nil {
			log.Printf("Failed to write audio data: %v", writeErr)
			break
		}
		time.Sleep(200 * time.Millisecond) // simulate real-time pacing
	}

	if err := recognizer.Stop(); err != nil {
		log.Printf("Failed to stop recognizer: %v", err)
	}
	fmt.Println("Processing complete.")
}

// parseRoles converts "name=url,name2=url2" into voiceprint enrollment roles.
func parseRoles(spec string) []v3.SpeakerRole {
	if spec == "" {
		return nil
	}
	var roles []v3.SpeakerRole
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, audioURL, ok := strings.Cut(entry, "=")
		if !ok {
			log.Fatalf("Invalid -roles entry %q, expected name=https://url", entry)
		}
		roles = append(roles, v3.SpeakerRole{
			RoleName: strings.TrimSpace(name),
			AudioURL: strings.TrimSpace(audioURL),
		})
	}
	return roles
}

func loadCredentialsFromEnv() {
	if SdkAppID == 0 {
		if v := os.Getenv(envSdkAppID); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				log.Fatalf("Error: %s must be an integer: %v", envSdkAppID, err)
			}
			SdkAppID = n
		}
	}
	if SecretKey == "" {
		SecretKey = os.Getenv(envSecretKey)
	}
}
