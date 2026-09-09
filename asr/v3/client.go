// client.go holds the shared offline HTTP plumbing: {auth, params} envelope
// construction and the POST round-trip used by SentenceRecognizer and
// FileRecognizer.
package v3

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/Tencent-RTC/trtc-asr-sdk-go/common"
	"github.com/google/uuid"
)

// newAuthBlock builds the auth block for an offline request. requestID is the
// UserSig identifier: the server binds the signature to it.
func newAuthBlock(credential *common.Credential, requestID string) (authBlock, error) {
	userSig := credential.UserSig
	if userSig == "" {
		var err error
		userSig, err = common.GenUserSig(credential.SdkAppID, credential.SecretKey, requestID, 86400)
		if err != nil {
			return authBlock{}, common.NewASRErrorf(common.ErrCodeAuthFailed, "generate user sig failed: %v", err)
		}
	}
	return authBlock{
		SdkAppID:  itoa(credential.SdkAppID),
		UserSig:   userSig,
		RequestID: requestID,
	}, nil
}

// marshalOfflineRequest builds the {"auth":..., "params":...} request body and
// returns it with the request_id used in the auth block.
func marshalOfflineRequest(credential *common.Credential, params interface{}) ([]byte, string, error) {
	requestID := uuid.New().String()
	auth, err := newAuthBlock(credential, requestID)
	if err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(offlineEnvelope{Auth: auth, Params: params})
	if err != nil {
		return nil, "", common.NewASRErrorf(common.ErrCodeInvalidParam, "marshal request failed: %v", err)
	}
	return body, requestID, nil
}

// postV3 sends one v3 offline request and returns the raw body plus the HTTP
// status. No custom headers and no query string: identity and telemetry all
// live in the body. Callers must judge the result by the body's numeric code —
// v3 deliberately answers some business errors with HTTP 200 (e.g. auth
// failure 4002), so the status code alone is not authoritative.
func postV3(client *http.Client, url string, body []byte) ([]byte, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, common.NewASRErrorf(common.ErrCodeInvalidParam, "create http request failed: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, 0, common.NewASRErrorf(common.ErrCodeConnectFailed, "http request failed: %v", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, httpResp.StatusCode, common.NewASRErrorf(common.ErrCodeReadFailed, "read response body failed: %v", err)
	}
	return respBody, httpResp.StatusCode, nil
}

// decodeFlatResponse unmarshals a v3 flat response body into out. Two failure
// shapes are rejected here:
//
//   - the body is not a JSON object (cannot be a v3 response);
//   - the HTTP status is not 2xx while the body carries no numeric code — a
//     gateway/LB JSON error page ({"error":"..."}, null) would otherwise
//     decode to Code==0 and be mistaken for success.
//
// A legitimate v3 endpoint always sends 2xx for Code==0, so the second guard
// never misfires on a real response.
func decodeFlatResponse(respBody []byte, status int, out interface{}) error {
	if err := json.Unmarshal(respBody, out); err != nil {
		return common.NewASRErrorf(common.ErrCodeServerError,
			"invalid response (http %d): %v", status, err)
	}
	var header struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	// out was just proven to be a JSON object, so this never fails.
	_ = json.Unmarshal(respBody, &header)
	if (status < 200 || status > 299) && header.Code == 0 {
		return common.NewASRErrorf(common.ErrCodeServerError,
			"http %d with non-v3 response body: %s", status, truncateBody(respBody))
	}
	return nil
}

// truncateBody caps the response body quoted in error messages.
func truncateBody(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}
