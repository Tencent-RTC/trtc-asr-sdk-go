// Package v3 implements the TRTC-ASR v3 protocol client.
//
// The v3 protocol restructures the wire format around two separated blocks:
// an "auth" block (sdkappid / usersig / request_id) consumed by the gateway's
// authentication layer, and a "params" block (engine, VAD, hotwords, filters,
// ...) consumed by the recognition layer. All field names are snake_case and
// responses are flat (no Response envelope) with numeric codes.
//
// Interfaces:
//
//   - SpeechRecognizer:   WebSocket wss://{host}/asr/v3 — the URL carries only
//     voice_id; auth and params travel in a single start frame sent right
//     after the handshake. The downlink message shape is unchanged from v2.
//   - SentenceRecognizer: HTTP POST /v3/transcribe — one-shot (≤60s).
//   - FileRecognizer:     HTTP POST /v3/create_transcription +
//     /v3/describe_transcription — async task for long audio.
//
// v3 uses SdkAppID as the only customer dimension; the Tencent Cloud AppID is
// not needed (use NewCredential, which takes just SdkAppID and SecretKey).
//
// The v2/v1 clients in package asr remain fully supported and unchanged.
//
// Usage:
//
//	credential := v3.NewCredential(sdkAppID, secretKey)
//	listener := &MyListener{}
//	recognizer := v3.NewSpeechRecognizer(credential, "16k_zh_en", listener)
//	recognizer.Start()
//	recognizer.Write(audioData)
//	recognizer.Stop()
package v3
