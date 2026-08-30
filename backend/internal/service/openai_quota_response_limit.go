package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/imroc/req/v3"
)

// openAIQuotaMaxResponseBodyBytes is deliberately smaller than the generic
// gateway response limit.  WHAM usage and reset-credit payloads are compact
// metadata responses; accepting a multi-megabyte body only increases the blast
// radius of a malformed or compromised relay response.
const openAIQuotaMaxResponseBodyBytes int64 = 1 << 20

// ErrOpenAIQuotaResponseBodyTooLarge is the stable low-level sentinel for a
// quota response that exceeds openAIQuotaMaxResponseBodyBytes.  Callers can
// use errors.Is even when the public service error adds an infraerrors cause.
var ErrOpenAIQuotaResponseBodyTooLarge = errors.New("openai quota response body too large")

// ErrOpenAIQuotaResetCreditEntriesTooMany is returned when a reset-credit
// payload contains more entries than the bounded parser contract permits.
// Truncating the list would make the available count and the consumable IDs
// disagree, so the parser fails closed instead.
var ErrOpenAIQuotaResetCreditEntriesTooMany = errors.New("openai quota reset-credit list exceeds entry limit")

var ErrOpenAIQuotaInvalidAccountID = infraerrors.New(
	http.StatusBadRequest,
	"OPENAI_QUOTA_INVALID_ACCOUNT_ID",
	"account id must be a positive integer",
)

// openAIQuotaResponseTooLargeError translates the internal sentinel to the
// stable HTTP-facing error used by quota handlers.  The cause is retained for
// errors.Is and diagnostics while the message never includes response bytes.
func openAIQuotaResponseTooLargeError(cause error) error {
	if cause == nil {
		cause = ErrOpenAIQuotaResponseBodyTooLarge
	}
	return infraerrors.Newf(
		http.StatusBadGateway,
		"OPENAI_QUOTA_RESPONSE_TOO_LARGE",
		"upstream quota response exceeds %d bytes",
		openAIQuotaMaxResponseBodyBytes,
	).WithCause(cause)
}

func isOpenAIQuotaResponseTooLarge(err error) bool {
	return errors.Is(err, ErrOpenAIQuotaResponseBodyTooLarge) ||
		errors.Is(err, ErrUpstreamResponseBodyTooLarge)
}

func isOpenAIQuotaResetCreditEntriesTooMany(err error) bool {
	return errors.Is(err, ErrOpenAIQuotaResetCreditEntriesTooMany)
}

// readOpenAIQuotaResponseBody consumes a req/v3 response only after the
// request has disabled req's automatic body read.  The transport (including
// its TLS impersonation, proxy, decompression and redirect policy) still runs
// unchanged; limiting at this point bounds the decoded body rather than just
// the compressed wire bytes.  One extra byte is read by the shared helper so
// an exactly-at-limit body remains valid while an over-limit body gets the
// stable sentinel.
func readOpenAIQuotaResponseBody(resp *req.Response) ([]byte, error) {
	if resp == nil || resp.Response == nil || resp.Body == nil {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := readUpstreamResponseBodyLimited(resp.Body, openAIQuotaMaxResponseBodyBytes)
	if errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
		return nil, fmt.Errorf("%w: limit=%d", ErrOpenAIQuotaResponseBodyTooLarge, openAIQuotaMaxResponseBodyBytes)
	}
	return body, err
}

// requestOpenAIQuotaJSON keeps req/v3's configured transport and request
// middleware while opting out of its unbounded automatic response read.  The
// caller receives the response metadata and the bounded raw body; successful
// JSON decoding is done here with the same standard encoding/json decoder used
// by req/v3's default client.
func requestOpenAIQuotaJSON(
	ctx context.Context,
	client *req.Client,
	method string,
	url string,
	headers map[string]string,
	requestBody any,
	result any,
) (*req.Response, []byte, error) {
	if client == nil {
		return nil, nil, fmt.Errorf("openai quota client is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	r := client.R().
		DisableAutoReadResponse().
		SetContext(ctx).
		SetHeaders(headers)
	if requestBody != nil {
		r.SetBody(requestBody)
	}
	resp, err := r.Send(method, url)
	if err != nil {
		return resp, nil, err
	}
	if resp == nil {
		return nil, nil, fmt.Errorf("openai quota request returned no response")
	}
	body, err := readOpenAIQuotaResponseBody(resp)
	if err != nil {
		return resp, nil, err
	}
	if result != nil && resp.IsSuccessState() && resp.StatusCode != http.StatusNoContent {
		if err := jsonUnmarshalQuotaBody(body, result); err != nil {
			return resp, body, err
		}
	}
	return resp, body, nil
}

// jsonUnmarshalQuotaBody is a small indirection so tests can exercise the
// bounded request helper without exposing req/v3 internals.  It deliberately
// rejects an empty success body in the same way json.Unmarshal does for a
// SetSuccessResult request.
func jsonUnmarshalQuotaBody(body []byte, result any) error {
	return json.Unmarshal(body, result)
}
