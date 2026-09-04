// params.go holds shared parameter validation for the recognizers.
//
// The service validates every parameter as well, but rejecting an obviously
// invalid value locally turns a remote 4001 ("参数不合法") into an immediate,
// descriptive error and avoids burning a connection or a task quota.
package asr

import (
	"net/url"
	"strings"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
)

// Server-side accepted ranges, kept in one place so streaming and file
// recognition validate identically.
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
			"SpeakerRoles/VoiceprintIds require SpeakerDiarization=3")
	}

	for i, role := range roles {
		if role.RoleName == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"SpeakerRoles[%d].RoleName is empty", i)
		}
		if err := validateEnrollmentURL(i, role.AudioUrl); err != nil {
			return err
		}
	}

	for i, id := range voiceprintIDs {
		if id == "" {
			return common.NewASRErrorf(common.ErrCodeInvalidParam,
				"VoiceprintIds[%d] is empty", i)
		}
	}

	return nil
}

// validateEnrollmentURL requires an absolute http(s) URL for enrollment audio.
//
// The URL is fetched by the ASR service, not by the SDK: this is a
// customer-facing client library, so it only rejects inputs that can never
// work (bad syntax, non-http scheme, missing host). Reachability and network
// policies belong to the service-side allow list.
func validateEnrollmentURL(index int, rawURL string) error {
	if strings.TrimSpace(rawURL) == "" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioUrl is empty", index)
	}
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioUrl is not a valid URL: %v", index, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioUrl must use http or https, got %q", index, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return common.NewASRErrorf(common.ErrCodeInvalidParam,
			"SpeakerRoles[%d].AudioUrl has no host", index)
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
