// Package main demonstrates how to use the TRTC-ASR Go SDK for real-time speech recognition.
//
// Usage:
//
//	go run main.go -f test.pcm
//	go run main.go -f test.pcm -e 16k_zh_en -c 1
//
// Prerequisites:
//  1. Get Tencent Cloud APPID: https://console.cloud.tencent.com/cam/capi
//  2. Create a TRTC application: https://console.cloud.tencent.com/trtc/app
//  3. Get SDKAppID and SDK secret key from the application overview page
//  4. Prepare a PCM audio file (16kHz, 16bit, mono)
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
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

// ===== Default Settings =====
var (
	EngineModelType = "bigmodel"
	SliceSize       = 6400 // bytes per audio chunk (200ms for 16kHz 16bit mono PCM)
)

const (
	envAppID     = "TRTC_ASR_APP_ID"
	envSdkAppID  = "TRTC_ASR_SDK_APP_ID"
	envSecretKey = "TRTC_ASR_SECRET_KEY"
)

// MySpeechRecognitionListener implements the SpeechRecognitionListener interface.
type MySpeechRecognitionListener struct {
	asr.UnimplementedSpeechRecognitionListener
	ID int
}

func (l *MySpeechRecognitionListener) OnRecognitionStart(resp *asr.SpeechRecognitionResponse) {
	log.Printf("[%d] Recognition started, voice_id: %s", l.ID, resp.VoiceID)
}

func (l *MySpeechRecognitionListener) OnSentenceBegin(resp *asr.SpeechRecognitionResponse) {
	log.Printf("[%d] Sentence begin, index: %d", l.ID, resp.Result.Index)
}

func (l *MySpeechRecognitionListener) OnRecognitionResultChange(resp *asr.SpeechRecognitionResponse) {
	log.Printf("[%d] Result change, index: %d, text: %s",
		l.ID, resp.Result.Index, resp.Result.VoiceTextStr)
}

func (l *MySpeechRecognitionListener) OnSentenceEnd(resp *asr.SpeechRecognitionResponse) {
	log.Printf("[%d] Sentence end, index: %d, lang: %s, text: %s",
		l.ID, resp.Result.Index, resp.Result.Language, resp.Result.VoiceTextStr)

	// With speaker diarization enabled, attribution lives in speaker_segments:
	// one sentence may contain several speakers, so a sentence-level speaker
	// would be ambiguous.
	for _, seg := range resp.Result.SpeakerSegments {
		label := seg.SpeakerName // only returned with diarization mode 3
		if label == "" {
			label = fmt.Sprintf("spk%d", seg.SpeakerID)
		}
		log.Printf("[%d]   [%s] %s (%d-%d ms, stable=%d)",
			l.ID, label, seg.Text, seg.StartTime, seg.EndTime, seg.StableFlag)
	}
}

func (l *MySpeechRecognitionListener) OnRecognitionComplete(resp *asr.SpeechRecognitionResponse) {
	log.Printf("[%d] Recognition complete, voice_id: %s", l.ID, resp.VoiceID)
}

func (l *MySpeechRecognitionListener) OnFail(resp *asr.SpeechRecognitionResponse, err error) {
	if resp != nil {
		log.Printf("[%d] Recognition failed, voice_id: %s, error: %v", l.ID, resp.VoiceID, err)
	} else {
		log.Printf("[%d] Recognition failed, error: %v", l.ID, err)
	}
}

func main() {
	concurrency := flag.Int("c", 1, "number of concurrent recognition sessions")
	loop := flag.Bool("l", false, "loop mode for stress testing")
	filePath := flag.String("f", "../test.pcm", "path to audio file (PCM or WAV)")
	engine := flag.String("e", "", "engine model type, required (e.g. bigmodel)")
	lang := flag.String("lang", "", "language hint; when omitted, the bigmodel engine uses zh")
	diarization := flag.Int("diarization", 0, "speaker diarization: 0=off, 1=cluster, 3=voiceprint roles")
	speakerNumber := flag.Int("speakers", 0, "expected speaker count hint (0=auto), only for -diarization=3")
	roleSpec := flag.String("roles", "", "voiceprint roles for -diarization=3: \"name=https://url,name2=https://url2\"")
	wordInfo := flag.Int("word-info", 0, "word-level timestamps: 0=off, 1=on, 2=with punctuation")
	vadLevel := flag.Int("vad-level", -1, "VAD profile: 0=high recall, 1=far-field; negative means unset")
	noiseThreshold := flag.Float64("noise-threshold", -1, "VAD noise threshold [0,4]; negative means unset")
	envFile := flag.String("env", "", "path to .env file to load credentials from (e.g. ../.env.test)")
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

	EngineModelType = *engine
	loadCredentialsFromEnv()
	if *envFile != "" {
		loadEnvFile(*envFile)
	}

	opts := recognizerOptions{
		language:       *lang,
		diarization:    *diarization,
		speakerNumber:  *speakerNumber,
		roles:          parseRoles(*roleSpec),
		wordInfo:       *wordInfo,
		vadLevel:       *vadLevel,
		noiseThreshold: *noiseThreshold,
	}

	if AppID == 0 || SdkAppID == 0 || SecretKey == "" {
		log.Fatal("Error: Please set AppID, SdkAppID and SecretKey in the code or via environment variables.\n\n" +
			"Steps:\n" +
			"  1. Get APPID from CAM Console: https://console.cloud.tencent.com/cam/capi\n" +
			"  2. Open TRTC Console: https://console.cloud.tencent.com/trtc/app\n" +
			"  3. Create or select an application\n" +
			"  4. Copy SDKAppID and SDK secret key from the application overview\n" +
			"  5. Fill in the credentials at the top of this file or export TRTC_ASR_APP_ID, TRTC_ASR_SDK_APP_ID and TRTC_ASR_SECRET_KEY.\n")
	}

	if _, err := os.Stat(*filePath); os.IsNotExist(err) {
		log.Fatalf("Error: Audio file not found: %s\n\nPlease provide a valid PCM/WAV audio file.", *filePath)
	}

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if *loop {
				for {
					processAudio(id, *filePath, opts)
					time.Sleep(time.Second)
				}
			} else {
				processAudio(id, *filePath, opts)
			}
		}(i)
	}
	wg.Wait()
}

// recognizerOptions groups the optional recognition settings so the worker
// signature stays readable as more knobs are added.
type recognizerOptions struct {
	language      string
	diarization   int
	speakerNumber int
	roles         []asr.SpeakerRole
	wordInfo      int
	// vadLevel / noiseThreshold are only applied when non-negative, mirroring
	// the SDK's "explicit 0 differs from unset" semantics.
	vadLevel       int
	noiseThreshold float64
}

// apply configures the recognizer with the requested options.
func (o recognizerOptions) apply(r *asr.SpeechRecognizer) {
	if o.language != "" {
		r.SetLanguage(o.language)
	}
	if o.wordInfo != 0 {
		r.SetWordInfo(o.wordInfo)
	}
	if o.diarization != 0 {
		r.SetSpeakerDiarization(o.diarization)
		r.SetSpeakerNumber(o.speakerNumber)
		if len(o.roles) > 0 {
			r.SetSpeakerRoles(o.roles)
		}
	}
	if o.vadLevel >= 0 {
		r.SetVadLevel(o.vadLevel)
	}
	if o.noiseThreshold >= 0 {
		r.SetNoiseThreshold(o.noiseThreshold)
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

func loadCredentialsFromEnv() {
	if AppID == 0 {
		AppID = parseEnvInt(envAppID)
	}
	if SdkAppID == 0 {
		SdkAppID = parseEnvInt(envSdkAppID)
	}
	if SecretKey == "" {
		SecretKey = os.Getenv(envSecretKey)
	}
}

func parseEnvInt(name string) int {
	value := os.Getenv(name)
	if value == "" {
		return 0
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("Error: %s must be an integer: %v", name, err)
	}
	return number
}

func processAudio(id int, filePath string, opts recognizerOptions) {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("[%d] Failed to open file: %v", id, err)
		return
	}
	defer file.Close()

	// If the file is a WAV, skip the header and stream raw PCM.
	sliceSize := SliceSize
	dataReader := io.Reader(file)
	if sampleRate, isWAV, _ := skipWAVHeader(file); isWAV {
		log.Printf("[%d] WAV detected (sample_rate=%d), header skipped", id, sampleRate)
		dataReader = file
		// 200ms slice = sampleRate * 2 bytes(16bit) * 0.2s
		sliceSize = sampleRate * 2 * 200 / 1000
		if sliceSize <= 0 {
			sliceSize = SliceSize
		}
	}

	credential := common.NewCredential(AppID, SdkAppID, SecretKey)
	listener := &MySpeechRecognitionListener{ID: id}
	recognizer := asr.NewSpeechRecognizer(credential, EngineModelType, listener)
	opts.apply(recognizer)

	if err := recognizer.Start(); err != nil {
		log.Printf("[%d] Failed to start recognizer: %v", id, err)
		return
	}

	buf := make([]byte, sliceSize)
	for {
		n, err := dataReader.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("[%d] Failed to read audio file: %v", id, err)
			break
		}

		if writeErr := recognizer.Write(buf[:n]); writeErr != nil {
			log.Printf("[%d] Failed to write audio data: %v", id, writeErr)
			break
		}

		time.Sleep(200 * time.Millisecond)
	}

	if err := recognizer.Stop(); err != nil {
		log.Printf("[%d] Failed to stop recognizer: %v", id, err)
	}

	fmt.Printf("[%d] Processing complete.\n", id)
}

// skipWAVHeader reads the RIFF/WAVE header and positions the file cursor at the
// start of the PCM data chunk. Returns (sampleRate, true, nil) for a valid WAV.
func skipWAVHeader(f *os.File) (int, bool, error) {
	// RIFF header: "RIFF" + 4-byte size + "WAVE" = 12 bytes
	header := make([]byte, 12)
	if _, err := io.ReadFull(f, header); err != nil {
		return 0, false, err
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		// Not a WAV — rewind so the caller can read raw PCM from the start.
		_, _ = f.Seek(0, io.SeekStart)
		return 0, false, nil
	}

	sampleRate := 16000
	// Walk chunks until we find "data".
	for {
		chunkHeader := make([]byte, 8)
		if _, err := io.ReadFull(f, chunkHeader); err != nil {
			return 0, false, err
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])

		if chunkID == "fmt " {
			// fmt chunk: audioFormat(2) + numChannels(2) + sampleRate(4) + ...
			fmtData := make([]byte, chunkSize)
			if _, err := io.ReadFull(f, fmtData); err != nil {
				return 0, false, err
			}
			if len(fmtData) >= 8 {
				sampleRate = int(binary.LittleEndian.Uint32(fmtData[4:8]))
			}
			continue
		}

		if chunkID == "data" {
			// File cursor now points at PCM samples.
			return sampleRate, true, nil
		}
		// Skip this chunk (chunkSize bytes, padded to even).
		skip := int64(chunkSize)
		if chunkSize%2 == 1 {
			skip++
		}
		if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
			return 0, false, err
		}
	}
}

// loadEnvFile reads a simple KEY = VALUE / KEY = "VALUE" file and populates
// AppID, SdkAppID, SecretKey when the corresponding env vars are not already set.
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("Error: cannot open env file %s: %v", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "Key = Value" or "Key = \"Value\""
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, "\"")

		switch strings.ToLower(key) {
		case "appid":
			if AppID == 0 {
				AppID = parseStrInt(val)
			}
		case "sdkappid":
			if SdkAppID == 0 {
				SdkAppID = parseStrInt(val)
			}
		case "secretkey":
			if SecretKey == "" {
				SecretKey = val
			}
		}
	}
}

func parseStrInt(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		log.Fatalf("Error: invalid integer %q: %v", s, err)
	}
	return n
}
