// params.go holds shared parameter validation for the v3 recognizers.
//
// The service validates every parameter as well, but rejecting an obviously
// invalid value locally turns a remote 4001 into an immediate, descriptive
// error and avoids burning a connection or a task quota. Ranges mirror the
// server-side v3 validator (asr-proxy validator_v3.go).
package v3

import (
	"net/url"
	"strings"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// Server-side accepted ranges.
const (
	minNoiseThreshold = 0.0
	maxNoiseThreshold = 4.0
)

// validateSpeakerDiarization checks the diarization mode and its enrollment
// input. roles/voiceprintIDs are only meaningful with mode 3, but supplying
// them for another mode is a caller mistake worth surfacing.
func validateSpeakerDiarization(mode, speakerNumber int, roles []SpeakerRole, voiceprintIDs []string) error {
	switch mode {
	case SpeakerDiarizationOff, SpeakerDiarizationCluster, SpeakerDiarizationVoiceprint:
	default:
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerDiarization must be 0 (off), 1 (cluster) or 3 (voiceprint), got %d", mode)
	}

	if speakerNumber < 0 {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerNumber must be >= 0 (0 = auto detection), got %d", speakerNumber)
	}

	if mode != SpeakerDiarizationVoiceprint && (len(roles) > 0 || len(voiceprintIDs) > 0) {
		return common.NewASRError(common.ErrCodeInvalidParam,
			"SpeakerRoles/VoiceprintIDs require SpeakerDiarization=3")
	}

	for i, role := range roles {
		if role.RoleName == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"SpeakerRoles[%d].RoleName is empty", i)
		}
		if err := validateEnrollmentURL(i, role.AudioURL); err != nil {
			return err
		}
	}

	for i, id := range voiceprintIDs {
		if id == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"VoiceprintIDs[%d] is empty", i)
		}
	}

	return nil
}

// validateEnrollmentURL requires an absolute http(s) URL for enrollment audio.
// The URL is fetched by the ASR service, not by the SDK: this client only
// rejects inputs that can never work (bad syntax, non-http scheme, no host).
func validateEnrollmentURL(index int, rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioURL is empty", index)
	}
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioURL is not a valid URL: %v", index, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioURL must use http or https, got %q", index, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioURL has no host", index)
	}
	return nil
}

// validateVadTuning checks the VAD profile and noise threshold.
func validateVadTuning(vadLevel *int, noiseThreshold *float64) error {
	if vadLevel != nil && *vadLevel != 0 && *vadLevel != 1 {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"VadLevel must be 0 (high recall) or 1 (far-field filtering), got %d", *vadLevel)
	}
	if noiseThreshold != nil {
		v := *noiseThreshold
		// NaN fails every comparison, so test the valid range positively.
		if !(v >= minNoiseThreshold && v <= maxNoiseThreshold) {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"NoiseThreshold must be between %.1f and %.1f, got %v",
				minNoiseThreshold, maxNoiseThreshold, v)
		}
	}
	return nil
}

// validateEnumOption checks a small enumerated option such as input_sample_rate.
func validateEnumOption(name string, value int, allowed ...int) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return common.NewASRErrorf(common.ErrCodeInvalidParam,
		"%s must be one of %v, got %d", name, allowed, value)
}

// validateAudioURLs checks the distributed-recording invariants the server
// enforces (asr-local-manager rectask_cluster.go
// validateDistributedAudioUrlsRequest): AudioURLs requires SourceType=0 with
// URL/Data left empty, and each item needs a unique non-negative Index and an
// absolute http(s) URL.
func validateAudioURLs(req *CreateTranscriptionRequest) error {
	if req.SourceType != SourceTypeURL || req.URL != "" || req.Data != "" {
		return common.NewASRError(common.ErrCodeInvalidParam,
			"AudioURLs cannot be used together with non-zero SourceType, URL or Data")
	}
	seen := make(map[int]struct{}, len(req.AudioURLs))
	for i, item := range req.AudioURLs {
		if item.Index < 0 {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"AudioURLs[%d].Index must be non-negative", i)
		}
		if _, ok := seen[item.Index]; ok {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"AudioURLs index duplicated: %d", item.Index)
		}
		seen[item.Index] = struct{}{}
		rawURL := strings.TrimSpace(item.URL)
		if rawURL == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"AudioURLs[%d].URL is required", i)
		}
		parsed, err := url.ParseRequestURI(rawURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"AudioURLs[%d].URL must be a valid http/https URL", i)
		}
	}
	return nil
}
