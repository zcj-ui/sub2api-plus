package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

const (
	openAIFirstOutputStageMemoryLimit        = 64 * 1024
	openAIFirstOutputStageMaxBytes           = 8 * 1024 * 1024
	openAIFirstOutputScannerFramingAllowance = 64
	openAIFirstOutputGuardQueueSize          = 1
	openAIDefaultStreamQueueSize             = 16
	// WSv2 keeps non-semantic lifecycle events private until the first visible
	// output.  Keep that pre-output queue bounded independently of the upstream
	// frame limit: a provider can legally send an arbitrary number of
	// response.created/in_progress-style events before producing a token.
	openAIWSBufferedStreamEventsMaxBytes           = openAIFirstOutputStageMaxBytes
	openAIWSBufferedStreamEventsMaxCount           = 256
	openAIWSBufferedStreamEventFrameOverhead int64 = int64(len("data: \n\n"))
)

var (
	errOpenAIFirstOutputStageLimit   = errors.New("openai first-output staging limit exceeded")
	errOpenAIFirstOutputScannerLimit = errors.New("openai pre-output scanner token limit exceeded")
	// errOpenAIPendingSSELinesLimit guards small in-memory pre-output buffers in
	// passthrough/Chat-Completions readers.  The full first-output stage already
	// uses the same 8 MiB ceiling; keeping one shared limit prevents a malformed
	// upstream that never emits visible output from growing an unbounded slice.
	errOpenAIPendingSSELinesLimit = errors.New("openai pending SSE lines limit exceeded")
)

// appendOpenAIPendingSSELine appends one line to a pre-output buffer while
// enforcing a byte ceiling.  The extra byte accounts for the newline emitted
// when the buffered line is later flushed.  On overflow the caller gets a
// typed sentinel and the buffer is cleared so rejected data is not retained.
func appendOpenAIPendingSSELine(lines *[]string, totalBytes *int64, line string, limit int64) error {
	if lines == nil || totalBytes == nil {
		return errOpenAIPendingSSELinesLimit
	}
	if limit <= 0 {
		limit = openAIFirstOutputStageMaxBytes
	}
	incoming := int64(len(line)) + 1
	if incoming < 0 || *totalBytes > limit || incoming > limit-*totalBytes {
		clearOpenAIPendingSSELines(lines, totalBytes)
		return fmt.Errorf("%w: limit=%d", errOpenAIPendingSSELinesLimit, limit)
	}
	*lines = append(*lines, line)
	*totalBytes += incoming
	return nil
}

// clearOpenAIPendingSSELines releases references held by a flushed/rejected
// pre-output buffer.  A reslice-to-zero would retain every large string until
// the enclosing request returns, which is particularly costly for long-lived
// streams that later produce normal output.
func clearOpenAIPendingSSELines(lines *[]string, totalBytes *int64) {
	if lines != nil && *lines != nil {
		clear(*lines)
		*lines = nil
	}
	if totalBytes != nil {
		*totalBytes = 0
	}
}

// appendOpenAIWSBufferedStreamEvent appends one pre-output WS event while
// enforcing both a byte and an event-count ceiling.  The byte accounting
// includes the small SSE framing that emitStreamMessage adds when the event is
// eventually released.  On overflow the queue is cleared so rejected payloads
// cannot remain reachable through a retained backing slice.
func appendOpenAIWSBufferedStreamEvent(events *[][]byte, totalBytes *int64, payload []byte, byteLimit int64, eventLimit int) error {
	if events == nil || totalBytes == nil {
		return errOpenAIPendingSSELinesLimit
	}
	if byteLimit <= 0 {
		byteLimit = openAIWSBufferedStreamEventsMaxBytes
	}
	if eventLimit <= 0 {
		eventLimit = openAIWSBufferedStreamEventsMaxCount
	}
	incoming := int64(len(payload)) + openAIWSBufferedStreamEventFrameOverhead
	if incoming < 0 || *totalBytes < 0 || *totalBytes > byteLimit ||
		incoming > byteLimit-*totalBytes || len(*events) >= eventLimit {
		buffered, queued := *totalBytes, len(*events)
		clearOpenAIWSBufferedStreamEvents(events, totalBytes)
		return fmt.Errorf("%w: buffered=%d incoming=%d limit=%d events=%d max_events=%d",
			errOpenAIPendingSSELinesLimit, buffered, incoming, byteLimit, queued, eventLimit)
	}
	*events = append(*events, append([]byte(nil), payload...))
	*totalBytes += incoming
	return nil
}

// clearOpenAIWSBufferedStreamEvents releases both the payload references and
// the backing slice after a flush/rejection.  Reslicing to zero would retain
// every pre-output event until the surrounding request returns.
func clearOpenAIWSBufferedStreamEvents(events *[][]byte, totalBytes *int64) {
	if events != nil && *events != nil {
		clear(*events)
		*events = nil
	}
	if totalBytes != nil {
		*totalBytes = 0
	}
}

type openAIFirstOutputStage struct {
	limit      int64
	size       int64
	memory     bytes.Buffer
	tempFile   *os.File
	tempPath   string
	createTemp func() (*os.File, error)
	removeFile func(string) error
	memoryOnly bool
	cleanupErr error
	closed     bool
}

func newOpenAIFirstOutputStage(limit int64) *openAIFirstOutputStage {
	if limit < 1 {
		limit = 1
	}
	return &openAIFirstOutputStage{
		limit:      limit,
		createTemp: func() (*os.File, error) { return os.CreateTemp("", "sub2api-openai-first-output-*") },
		removeFile: os.Remove,
		memoryOnly: runtime.GOOS == "windows",
	}
}

func newDefaultOpenAIFirstOutputStage() *openAIFirstOutputStage {
	return newOpenAIFirstOutputStage(openAIFirstOutputStageMaxBytes)
}

func openAIFirstOutputEventQueueSize(guardFirstOutput bool) int {
	if guardFirstOutput {
		return openAIFirstOutputGuardQueueSize
	}
	return openAIDefaultStreamQueueSize
}

func openAIFirstOutputDynamicScanLines(guardActive *atomic.Bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		if err != nil || guardActive == nil || !guardActive.Load() {
			return advance, token, err
		}
		limit := openAIFirstOutputStageMaxBytes + openAIFirstOutputScannerFramingAllowance
		if token != nil {
			if len(token) > limit {
				return 0, nil, errOpenAIFirstOutputScannerLimit
			}
			return advance, token, nil
		}
		// At the limit with no delimiter, another byte would necessarily exceed
		// the guarded token budget. Fail before Scanner grows toward MaxLineSize.
		if len(data) >= limit {
			return 0, nil, errOpenAIFirstOutputScannerLimit
		}
		return advance, token, nil
	}
}

func (s *openAIFirstOutputStage) Buffered() int64 {
	if s == nil {
		return 0
	}
	return s.size
}

func (s *openAIFirstOutputStage) WriteString(value string) (int, error) {
	if err := s.prepareWrite(len(value)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.WriteString(value)
	} else {
		n, err = io.WriteString(s.tempFile, value)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) Write(p []byte) (int, error) {
	if err := s.prepareWrite(len(p)); err != nil {
		return 0, err
	}
	var n int
	var err error
	if s.tempFile == nil {
		n, err = s.memory.Write(p)
	} else {
		n, err = s.tempFile.Write(p)
	}
	s.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("write first-output stage: %w", err)
	}
	return n, nil
}

func (s *openAIFirstOutputStage) prepareWrite(incoming int) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if int64(incoming) > s.limit-s.size {
		return fmt.Errorf("%w: buffered=%d incoming=%d limit=%d", errOpenAIFirstOutputStageLimit, s.size, incoming, s.limit)
	}
	if s.tempFile != nil || s.memoryOnly || s.size+int64(incoming) <= openAIFirstOutputStageMemoryLimit {
		return nil
	}
	file, err := s.createTemp()
	if err != nil {
		return fmt.Errorf("create first-output spool: %w", err)
	}
	path := file.Name()
	// Unlink before writing any request data. Unix keeps the file descriptor
	// readable, while crashes and SIGKILL cannot leave a named plaintext spool.
	if unlinkErr := s.removeFile(path); unlinkErr != nil {
		closeErr := file.Close()
		removeErr := s.removeFile(path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		s.memoryOnly = true
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = path
		}
		s.cleanupErr = errors.Join(
			s.cleanupErr,
			fmt.Errorf("unlink first-output spool before use: %w", unlinkErr),
			closeErr,
			removeErr,
		)
		return nil
	}
	if _, err := file.Write(s.memory.Bytes()); err != nil {
		_ = file.Close()
		return fmt.Errorf("initialize first-output spool: %w", err)
	}
	s.tempFile = file
	s.tempPath = path
	// The spool now owns the staged bytes; release the in-memory backing array
	// immediately instead of retaining up to the full stage limit until Close.
	s.memory = bytes.Buffer{}
	return nil
}

func (s *openAIFirstOutputStage) CommitTo(dst io.Writer) error {
	if s == nil || s.closed {
		return os.ErrClosed
	}
	if s.tempFile == nil {
		if _, err := io.Copy(dst, bytes.NewReader(s.memory.Bytes())); err != nil {
			return err
		}
	} else {
		if _, err := s.tempFile.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek first-output spool: %w", err)
		}
		if _, err := io.CopyN(dst, s.tempFile, s.size); err != nil {
			return err
		}
	}
	if err := s.Close(); err != nil {
		// Delivery succeeded. Preserve cleanup failures for the handler's deferred
		// cleanup/logging pass instead of turning committed bytes into a stream error.
		s.cleanupErr = errors.Join(s.cleanupErr, err)
	}
	return nil
}

func (s *openAIFirstOutputStage) Close() error {
	if s == nil {
		return nil
	}
	if s.closed && s.tempFile == nil && s.tempPath == "" && s.cleanupErr == nil {
		return nil
	}
	s.closed = true
	s.size = 0
	// Resetting a bytes.Buffer keeps its backing array alive.  First-output
	// staging can reach several MiB, so drop the buffer value itself on every
	// close to make the memory promptly collectible on long-lived streams.
	s.memory = bytes.Buffer{}
	closeErr := s.cleanupErr
	s.cleanupErr = nil
	if s.tempFile != nil {
		closeErr = errors.Join(closeErr, s.tempFile.Close())
		s.tempFile = nil
	}
	if s.tempPath != "" {
		removeErr := s.removeFile(s.tempPath)
		if removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
			s.tempPath = ""
		} else {
			closeErr = errors.Join(closeErr, removeErr)
		}
	}
	return closeErr
}

func (s *OpenAIGatewayService) openAIFirstOutputTimeout(reasoningEffort string) time.Duration {
	if s == nil || s.cfg == nil || s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds <= 0 {
		return 0
	}
	seconds := s.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds
	switch strings.ToLower(strings.TrimSpace(reasoningEffort)) {
	case "high", "xhigh", "max":
		if override := s.cfg.Gateway.OpenAIHighEffortFirstOutputTimeoutSeconds; override > 0 {
			seconds = override
		}
	}
	return time.Duration(seconds) * time.Second
}

func (s *OpenAIGatewayService) newOpenAIFirstOutputTimeoutError(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	originalModel string,
	reasoningEffort string,
	timeout time.Duration,
	phase string,
	responseHeaders http.Header,
) *UpstreamFailoverError {
	elapsed := time.Since(startTime)
	logger.LegacyPrintf(
		"service.openai_gateway",
		"OpenAI first output timeout: account=%d model=%s effort=%s phase=%s elapsed=%s limit=%s",
		account.ID, originalModel, reasoningEffort, phase, elapsed, timeout,
	)
	requestID := strings.TrimSpace(responseHeaders.Get("x-request-id"))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:   opsUpstreamProxyID(account),
		ProxyName: opsUpstreamProxyName(account),
		Platform:  account.Platform, AccountID: account.ID, AccountName: account.Name,
		UpstreamStatusCode: http.StatusGatewayTimeout, UpstreamRequestID: requestID,
		Kind: "first_output_timeout", Message: "OpenAI upstream produced no semantic output before the deadline",
		Detail: fmt.Sprintf("phase=%s elapsed_ms=%d timeout_ms=%d", phase, elapsed.Milliseconds(), timeout.Milliseconds()),
	})
	if s.rateLimitService != nil {
		s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusGatewayTimeout,
		ResponseBody:    []byte(`{"error":{"type":"first_output_timeout","message":"Upstream produced no output before the deadline"}}`),
		ResponseHeaders: responseHeaders.Clone(), SafeToFailoverAfterWrite: true,
	}
}

type openAIFirstOutputHeaderGuard struct {
	cancel  context.CancelFunc
	release context.CancelFunc
	timer   *time.Timer
	fired   chan struct{}
	once    sync.Once
}

func newOpenAIFirstOutputHeaderGuard(
	ctx context.Context,
	release context.CancelFunc,
	deadline time.Time,
) (context.Context, *openAIFirstOutputHeaderGuard) {
	guardedCtx, cancel := context.WithCancel(ctx)
	guard := &openAIFirstOutputHeaderGuard{cancel: cancel, release: release, fired: make(chan struct{})}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		remaining = time.Nanosecond
	}
	guard.timer = time.AfterFunc(remaining, func() {
		close(guard.fired)
		cancel()
	})
	return guardedCtx, guard
}

func (g *openAIFirstOutputHeaderGuard) stopHeaderWait() bool {
	if g.timer.Stop() {
		return false
	}
	<-g.fired
	return true
}

func (g *openAIFirstOutputHeaderGuard) close() {
	g.once.Do(func() {
		g.timer.Stop()
		g.cancel()
		g.release()
	})
}

type openAIRequestContextReadCloser struct {
	io.ReadCloser
	cleanup func()
	once    sync.Once
	err     error
}

func (r *openAIRequestContextReadCloser) Close() error {
	r.once.Do(func() {
		r.cleanup()
		r.err = r.ReadCloser.Close()
	})
	return r.err
}
