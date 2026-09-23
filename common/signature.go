package common

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Speaker diarization modes for the speaker_diarization parameter.
const (
	// SpeakerDiarizationOff disables speaker diarization (server default).
	SpeakerDiarizationOff = 0
	// SpeakerDiarizationCluster enables anonymous clustering: speakers are
	// numbered from 1 within the current session, -1 means unknown.
	SpeakerDiarizationCluster = 1
	// SpeakerDiarizationVoiceprint enables voiceprint-based role
	// authentication. Combine with SpeakerRoles (temporary enrollment audio)
	// and/or VoiceprintIDs (pre-registered voiceprints) so that recognized
	// speakers carry their role name in speaker_name.
	SpeakerDiarizationVoiceprint = 3
)

// SpeakerRole is a temporary voiceprint enrollment entry used with
// speaker_diarization=3. RoleName is echoed back by the server as
// speaker_name on the matched words / speaker segments.
//
// The JSON field names intentionally match the server-side contract
// (CamelCase) for both the streaming speaker_roles query parameter and the
// CreateRecTask SpeakerRoles body field.
type SpeakerRole struct {
	// RoleName is the caller-defined speaker label (e.g. "teacher").
	RoleName string `json:"RoleName"`
	// AudioUrl points to the enrollment audio for this role.
	AudioUrl string `json:"AudioUrl"`
}

// SignatureParams holds URL query parameters for the ASR WebSocket request.
// The "secretid" URL parameter is required by the protocol but internally
// populated with AppID — users do not need to provide a separate SecretID.
// The "signature" parameter is set to the UserSig value per protocol spec.
//
// Authentication identity travels in the URL instead of HTTP headers: the
// gateway accepts the "sdkappid" / "usersig" query parameters, and browsers
// cannot attach custom headers to a native WebSocket handshake.
type SignatureParams struct {
	AppID           int
	Timestamp       int64
	Expired         int64
	Nonce           int
	EngineModelType string
	VoiceID         string
	VoiceFormat     int
	NeedVad         int

	// SdkAppID is the TRTC application ID, sent as the "sdkappid" query
	// parameter. 0 means not configured.
	SdkAppID int

	// Optional parameters
	HotwordID       string
	HotwordList     string // temporary inline hotwords: "word|weight,word|weight"
	CustomizationID string
	ReplaceTextID   string // replacement word table ID
	FilterDirty     int
	FilterModal     int
	FilterPunc      int
	ConvertNumMode  int
	WordInfo        int
	VadSilenceTime  int
	MaxSpeakTime    int
	InputSampleRate int    // 8000: feed 8kHz PCM to a 16k engine (upsampled server-side)
	Language        string // bigmodel engine language hint (e.g. "ms", "zh", "auto")

	// FilterEmptyResult controls empty-result callbacks: 0=deliver empty
	// results, 1=skip them (server default). nil leaves the parameter out.
	FilterEmptyResult *int

	// VadLevel selects the VAD profile: 0=high recall, 1=far-field filtering
	// (server default). nil leaves the parameter out, so an explicit 0 is
	// distinguishable from "not configured".
	VadLevel *int

	// NoiseThreshold fine-tunes VAD noise suppression, range [0, 4]. When set
	// it overrides the profile selected by VadLevel. nil leaves the parameter
	// out (0 is a valid, meaningful value).
	NoiseThreshold *float64

	// SpeakerDiarization enables speaker diarization: 0=off (default),
	// 1=anonymous clustering, 3=voiceprint role authentication.
	SpeakerDiarization int

	// SpeakerNumber hints the expected speaker count; 0=auto detection
	// (default). Sent whenever diarization is enabled: the server feeds it
	// into online clustering for both modes.
	SpeakerNumber int

	// SpeakerRoles carries temporary voiceprint enrollment audio, serialized
	// into the speaker_roles JSON array. Only sent when SpeakerDiarization is 3.
	SpeakerRoles []SpeakerRole

	// VoiceprintIDs lists pre-registered voiceprint IDs, serialized into the
	// voiceprintids JSON array. Only sent when SpeakerDiarization is 3.
	VoiceprintIDs []string

	// EnableSpeakerContext makes the diarization session resumable
	// ("说话人分离断点续传"): the server stores the settled speaker anchors and
	// hands back an opaque speaker_context_id on the first response, which a
	// later connection passes back to keep the same speakers on the same ids.
	// 0=off (default), 1=report the restore status on the first response,
	// 2=answer with the id only (the restore happens in the background).
	// Requires SpeakerDiarization 1 or 3.
	EnableSpeakerContext int

	// SpeakerContextID carries the id issued by a previous session. Only sent
	// together with EnableSpeakerContext; an expired or unknown id simply
	// starts a new session instead of failing.
	SpeakerContextID string
}

// NewSignatureParams creates SignatureParams with sensible defaults.
func NewSignatureParams(appID int, engineModelType, voiceID string) *SignatureParams {
	now := time.Now().Unix()
	return &SignatureParams{
		AppID:           appID,
		Timestamp:       now,
		Expired:         now + 86400,
		Nonce:           rand.Intn(9999999) + 1,
		EngineModelType: engineModelType,
		VoiceID:         voiceID,
		VoiceFormat:     1, // pcm
		NeedVad:         1,
		ConvertNumMode:  1,
	}
}

// BuildQueryString constructs the URL query string with all parameters (without signature).
func (p *SignatureParams) BuildQueryString() string {
	params := p.toMap()
	return encodeParams(params)
}

// BuildQueryStringWithSignature constructs the URL query string with signature set to the given userSig.
// Per protocol: "signature" value equals the UserSig. The same value is also
// sent as the "usersig" query parameter, which the gateway reads when the
// X-TRTC-UserSig header is absent (e.g. browser WebSocket clients).
func (p *SignatureParams) BuildQueryStringWithSignature(userSig string) string {
	params := p.toMap()
	params["signature"] = userSig
	params["usersig"] = userSig
	return encodeParams(params)
}

func (p *SignatureParams) toMap() map[string]string {
	// "secretid" is required by protocol; internally use AppID as its value.
	m := map[string]string{
		"secretid":          fmt.Sprintf("%d", p.AppID),
		"timestamp":         fmt.Sprintf("%d", p.Timestamp),
		"expired":           fmt.Sprintf("%d", p.Expired),
		"nonce":             fmt.Sprintf("%d", p.Nonce),
		"engine_model_type": p.EngineModelType,
		"voice_id":          p.VoiceID,
		"voice_format":      fmt.Sprintf("%d", p.VoiceFormat),
		"needvad":           fmt.Sprintf("%d", p.NeedVad),
	}
	// SDK self-identification for server-side diagnostics. Not part of the
	// signature (the signature is the UserSig), so it is safe to append.
	for k, v := range SDKReportParams() {
		m[k] = v
	}
	if p.SdkAppID > 0 {
		m["sdkappid"] = fmt.Sprintf("%d", p.SdkAppID)
	}

	if p.HotwordID != "" {
		m["hotword_id"] = p.HotwordID
	}
	if p.HotwordList != "" {
		m["hotword_list"] = p.HotwordList
	}
	if p.CustomizationID != "" {
		m["customization_id"] = p.CustomizationID
	}
	if p.ReplaceTextID != "" {
		m["replace_text_id"] = p.ReplaceTextID
	}
	if p.FilterDirty != 0 {
		m["filter_dirty"] = fmt.Sprintf("%d", p.FilterDirty)
	}
	if p.FilterModal != 0 {
		m["filter_modal"] = fmt.Sprintf("%d", p.FilterModal)
	}
	if p.FilterPunc != 0 {
		m["filter_punc"] = fmt.Sprintf("%d", p.FilterPunc)
	}
	if p.FilterEmptyResult != nil {
		m["filter_empty_result"] = fmt.Sprintf("%d", *p.FilterEmptyResult)
	}
	if p.ConvertNumMode != 0 {
		m["convert_num_mode"] = fmt.Sprintf("%d", p.ConvertNumMode)
	}
	if p.WordInfo != 0 {
		m["word_info"] = fmt.Sprintf("%d", p.WordInfo)
	}
	if p.VadSilenceTime != 0 {
		m["vad_silence_time"] = fmt.Sprintf("%d", p.VadSilenceTime)
	}
	if p.MaxSpeakTime != 0 {
		m["max_speak_time"] = fmt.Sprintf("%d", p.MaxSpeakTime)
	}
	if p.InputSampleRate != 0 {
		m["input_sample_rate"] = fmt.Sprintf("%d", p.InputSampleRate)
	}
	// VadLevel and NoiseThreshold are tri-state: an explicit 0 differs from
	// "not configured" (the server defaults vad_level to 1), so they are only
	// emitted when the caller set them.
	if p.VadLevel != nil {
		m["vad_level"] = fmt.Sprintf("%d", *p.VadLevel)
	}
	if p.NoiseThreshold != nil {
		m["noise_threshold"] = strconv.FormatFloat(*p.NoiseThreshold, 'f', 3, 64)
	}
	if p.SpeakerDiarization != 0 {
		m["speaker_diarization"] = fmt.Sprintf("%d", p.SpeakerDiarization)
		if p.SpeakerNumber != 0 {
			m["speaker_number"] = fmt.Sprintf("%d", p.SpeakerNumber)
		}
	}
	if p.EnableSpeakerContext != 0 {
		m["enable_speaker_context"] = fmt.Sprintf("%d", p.EnableSpeakerContext)
		if p.SpeakerContextID != "" {
			m["speaker_context_id"] = p.SpeakerContextID
		}
	}
	// speaker_roles / voiceprintids only apply to the voiceprint role
	// authentication mode.
	if p.SpeakerDiarization == SpeakerDiarizationVoiceprint {
		if len(p.SpeakerRoles) > 0 {
			if rolesJSON, err := json.Marshal(p.SpeakerRoles); err == nil {
				m["speaker_roles"] = string(rolesJSON)
			}
		}
		if len(p.VoiceprintIDs) > 0 {
			if idsJSON, err := json.Marshal(p.VoiceprintIDs); err == nil {
				m["voiceprintids"] = string(idsJSON)
			}
		}
	}
	if p.Language != "" {
		m["language"] = p.Language
	}

	return m
}

func encodeParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, url.QueryEscape(params[k])))
	}
	return strings.Join(parts, "&")
}
