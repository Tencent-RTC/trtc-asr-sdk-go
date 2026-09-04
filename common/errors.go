package common

import "fmt"

// Error codes for TRTC-ASR SDK.
const (
	ErrCodeInvalidParam   = 1001
	ErrCodeConnectFailed  = 1002
	ErrCodeWriteFailed    = 1003
	ErrCodeReadFailed     = 1004
	ErrCodeAuthFailed     = 1005
	ErrCodeTimeout        = 1006
	ErrCodeServerError    = 1007
	ErrCodeAlreadyStarted = 1008
	ErrCodeNotStarted     = 1009
	ErrCodeAlreadyStopped = 1010
)

// ASRError represents an error returned by the TRTC-ASR service or SDK.
type ASRError struct {
	Code    int
	Message string
}

func (e *ASRError) Error() string {
	return fmt.Sprintf("trtc-asr error [%d]: %s", e.Code, e.Message)
}

// NewASRError creates a new ASRError.
func NewASRError(code int, message string) *ASRError {
	return &ASRError{Code: code, Message: message}
}

// NewASRErrorf creates a new ASRError with formatted message.
func NewASRErrorf(code int, format string, args ...interface{}) *ASRError {
	return &ASRError{Code: code, Message: fmt.Sprintf(format, args...)}
}
