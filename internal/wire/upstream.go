package wire

// UpstreamError carries an upstream HTTP status (plus CodeBuddy's business
// code, when known) out of the executor so the host can classify the failure.
// The host's decodeEnvelopeResult only surfaces envelope.Error.HTTPStatus, so
// this type is what makes a 429 reach MarkResult as a quota signal instead of
// being swallowed as a plain plugin error.
type UpstreamError struct {
	Code      string // CodeBuddy business code, e.g. "11101" or "" for HTTP-level failures
	Message   string
	Status    int // upstream HTTP status, 0 if unknown
	Retryable bool
}

func (e *UpstreamError) Error() string {
	return e.Message
}

func NewUpstreamError(status int, code, message string) *UpstreamError {
	return &UpstreamError{Status: status, Code: code, Message: message}
}

func (e *UpstreamError) WithRetryable(retryable bool) *UpstreamError {
	e.Retryable = retryable
	return e
}
