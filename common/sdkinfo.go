// sdkinfo.go carries the SDK's self-identification.
//
// Every request (WebSocket handshake and HTTP API calls) reports which SDK
// language, version and OS platform produced it. Without this, a customer
// issue can only be traced to an AppID — not to the concrete client build that
// triggered it, which is what makes cross-version regressions diagnosable.
//
// The values travel as URL query parameters rather than headers because a
// browser-originated WebSocket handshake cannot set custom headers, and the
// three transports must report identically.
package common

import (
	"fmt"
	"net/url"
	"runtime"
	"sort"
	"strings"
)

const (
	// SDKVersion is the released version of this SDK. Keep in sync with the
	// version recorded in CHANGELOG.md.
	SDKVersion = "1.2.3"

	// SDKLanguage identifies the SDK implementation language.
	SDKLanguage = "go"

	// SDKType distinguishes this family of SDKs from the client-side ones. All
	// six language bindings here run server-side, so the value is constant;
	// it exists so server-side telemetry can bucket traffic the same way it
	// does for the mobile/desktop client SDKs.
	SDKType = "server"
)

// SDKPlatform reports the OS platform the SDK is running on, normalized to the
// vocabulary the service expects: windows, linux, mac, android, ios. Any other
// GOOS is reported verbatim so a new platform shows up in telemetry instead of
// being silently misattributed.
func SDKPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "mac"
	case "windows":
		return "windows"
	case "linux":
		return "linux"
	case "android":
		return "android"
	case "ios":
		return "ios"
	default:
		return runtime.GOOS
	}
}

// SDKReportParams returns the SDK identification parameters shared by every
// transport.
func SDKReportParams() map[string]string {
	return map[string]string{
		"platform": SDKPlatform(),
		"sdk_lang": SDKLanguage,
		"sdk_type": SDKType,
		"version":  SDKVersion,
	}
}

// SDKReportQuery returns the SDK identification parameters as an encoded query
// fragment (no leading "&"), for the transports that build their URL by string
// concatenation.
func SDKReportQuery() string {
	params := SDKReportParams()
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, url.QueryEscape(params[k])))
	}
	return strings.Join(parts, "&")
}
