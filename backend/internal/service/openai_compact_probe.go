package service

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	AccountTestModeDefault = "default"
	AccountTestModeCompact = "compact"

	openAICompactProbeProtocolVersion = 2
	openAICompactProbeMaxAge          = 30 * 24 * time.Hour
	openAICompactProbeMaxBodyBytes    = 2 << 20

	openAICompactProbeSupportedExtraKey          = "openai_compact_supported"
	openAICompactProbeVersionExtraKey            = "openai_compact_probe_version"
	openAICompactProbeCheckedAtExtraKey          = "openai_compact_checked_at"
	openAICompactProbeLastStatusExtraKey         = "openai_compact_last_status"
	openAICompactProbeLastErrorExtraKey          = "openai_compact_last_error"
	OpenAICompactProbeObservedAtUnixNanoExtraKey = "openai_compact_probe_observed_at_unix_nano"
)

func normalizeAccountTestMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), AccountTestModeCompact) {
		return AccountTestModeCompact
	}
	return AccountTestModeDefault
}

// createOpenAICompactProbePayload mirrors Codex RemoteCompactionV2: the final
// input item is compaction_trigger and the request is streamed.
func createOpenAICompactProbePayload(model string, isOAuth bool) map[string]any {
	payload := map[string]any{
		"model":               strings.TrimSpace(model),
		"instructions":        "You are a helpful coding assistant.",
		"tools":               []any{},
		"parallel_tool_calls": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "Respond with OK."},
			map[string]any{"type": "compaction_trigger"},
		},
		"stream": true,
	}
	if isOAuth {
		payload["store"] = false
		metadata, _ := json.Marshal(map[string]any{
			"request_kind": "compaction",
			"compaction": map[string]any{
				"trigger": "manual", "reason": "user_requested",
				"implementation": "responses_compaction_v2",
				"phase":          "standalone_turn", "strategy": "memento",
			},
		})
		payload["client_metadata"] = map[string]any{"x-codex-turn-metadata": string(metadata)}
	}
	return payload
}

type openAICompactProbeEvent struct {
	Type string `json:"type"`
	Item struct {
		Type string `json:"type"`
	} `json:"item"`
}

// evaluateOpenAICompactProbeSSE follows the official collector semantics:
// only output_item.done counts, exactly one compaction item is required, and a
// response.completed terminal is mandatory. Bytes after the first terminal
// are ignored.
func evaluateOpenAICompactProbeSSE(body []byte) (openAIProbeVerdict, string) {
	if len(bytes.TrimSpace(body)) == 0 {
		return openAIProbeVerdictUnknown, "empty compact probe response"
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Split(splitOpenAISSELines)
	scanner.Buffer(make([]byte, 64*1024), len(body)+1)
	dataLines := make([]string, 0, 1)
	totalDone, compactionDone, completed := 0, 0, 0
	terminalFailure := ""
	terminalSeen, streamDone, seenSSEField := false, false, false
	protocolErr := ""
	firstLine := true
	consumeEvent := func() {
		if protocolErr != "" || len(dataLines) == 0 {
			dataLines = dataLines[:0]
			return
		}
		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if payload == "" || terminalSeen || streamDone {
			return
		}
		if payload == "[DONE]" {
			streamDone = true
			return
		}
		var event openAICompactProbeEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			protocolErr = "invalid compact probe SSE JSON"
			return
		}
		switch strings.TrimSpace(event.Type) {
		case "response.output_item.done":
			totalDone++
			if isResponsesCompactionItemType(event.Item.Type) {
				compactionDone++
			}
		case "response.completed":
			terminalSeen = true
			completed++
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
			terminalSeen = true
			terminalFailure = strings.TrimSpace(event.Type)
		}
	}
	for scanner.Scan() {
		if terminalSeen || streamDone {
			continue
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			consumeEvent()
			continue
		}
		if strings.HasPrefix(line, ":") {
			seenSSEField = true
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field, value = line, ""
		}
		seenSSEField = true
		if field == "data" {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return openAIProbeVerdictUnknown, "failed to parse compact probe SSE"
	}
	consumeEvent()
	if !seenSSEField {
		return openAIProbeVerdictUnknown, "compact probe response is not SSE"
	}
	if protocolErr != "" {
		return openAIProbeVerdictUnknown, protocolErr
	}
	if terminalFailure != "" {
		return openAIProbeVerdictUnknown, "compact probe terminated with " + terminalFailure
	}
	if completed != 1 {
		return openAIProbeVerdictUnknown, "compact probe must contain exactly one response.completed event"
	}
	if compactionDone > 1 {
		return openAIProbeVerdictUnknown, "compact probe produced multiple compaction output items"
	}
	if compactionDone == 0 {
		return openAIProbeVerdictUnsupported, "completed response did not produce a compaction output item (output items=" + strconv.Itoa(totalDone) + ")"
	}
	return openAIProbeVerdictSupported, ""
}

// splitOpenAISSELines accepts LF, CRLF, and lone CR records.
func splitOpenAISSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	for i, b := range data {
		switch b {
		case '\n':
			return i + 1, data[:i], nil
		case '\r':
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if i+1 < len(data) && data[i+1] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func shouldMarkOpenAICompactUnsupported(status int, body []byte) bool {
	switch status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	case http.StatusBadRequest, http.StatusForbidden, http.StatusUnprocessableEntity:
		lower := strings.ToLower(strings.TrimSpace(extractUpstreamErrorMessage(body) + " " + string(body)))
		if strings.Contains(lower, "compact") {
			for _, keyword := range []string{"unsupported", "not support", "does not support", "not available", "disabled"} {
				if strings.Contains(lower, keyword) {
					return true
				}
			}
		}
	}
	return false
}

func evaluateOpenAICompactProbeHTTP(resp *http.Response, body []byte) (openAIProbeVerdict, string) {
	if resp == nil {
		return openAIProbeVerdictUnknown, "compact probe failed"
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return evaluateOpenAICompactProbeSSE(body)
	}
	if shouldMarkOpenAICompactUnsupported(resp.StatusCode, body) {
		return openAIProbeVerdictUnsupported, strings.TrimSpace(extractUpstreamErrorMessage(body))
	}
	return openAIProbeVerdictUnknown, strings.TrimSpace(extractUpstreamErrorMessage(body))
}

// openAICompactProbeFoundCompactionItem is retained for callers that only
// need the historical best-effort diagnostic. Production capability decisions
// use evaluateOpenAICompactProbeHTTP above and therefore remain strict.
func openAICompactProbeFoundCompactionItem(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	text := string(body)
	if _, found := findRawCompactionItemFromSSE(text); found {
		return true
	}
	if finalResponse, ok := extractCodexFinalResponse(text); ok && responsesOutputHasCompactionItem(finalResponse) {
		return true
	}
	return responsesOutputHasCompactionItem(body)
}

func openAICompactProbeSnapshotFresh(extra map[string]any, now time.Time) bool {
	version, exists := extra[openAICompactProbeVersionExtraKey]
	if !exists || !numericExtraEquals(version, openAICompactProbeProtocolVersion) {
		return false
	}
	checkedAt, ok := extra[openAICompactProbeCheckedAtExtraKey].(string)
	if !ok {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(checkedAt))
	if err != nil || parsed.After(now.Add(5*time.Minute)) {
		return false
	}
	return now.Sub(parsed) <= openAICompactProbeMaxAge
}

func numericExtraEquals(value any, expected int) bool {
	switch typed := value.(type) {
	case int:
		return typed == expected
	case int64:
		return typed == int64(expected)
	case float64:
		return typed == float64(expected)
	case json.Number:
		actual, err := typed.Int64()
		return err == nil && actual == int64(expected)
	default:
		return false
	}
}

func openAICompactProbeReadError(err error) string {
	if errors.Is(err, errOpenAIProbeBodyTooLarge) {
		return "compact probe response exceeded 2 MiB limit"
	}
	if err == nil {
		return ""
	}
	return "failed to read compact probe response: " + err.Error()
}

// The variadic tail preserves the old internal helper call shape while
// accepting the strict v2 shape used by production and newer tests.
func buildOpenAICompactProbeExtraUpdates(resp *http.Response, body []byte, probeErr error, args ...any) map[string]any {
	if len(args) >= 4 {
		verdict, verdictOK := args[0].(openAIProbeVerdict)
		reason, reasonOK := args[1].(string)
		startedAt, startedOK := args[2].(time.Time)
		now, nowOK := args[3].(time.Time)
		if verdictOK && reasonOK && startedOK && nowOK {
			return buildOpenAICompactProbeExtraUpdatesV2(resp, body, probeErr, verdict, reason, startedAt, now)
		}
	}
	var found bool
	var now time.Time
	if len(args) > 0 {
		found, _ = args[0].(bool)
	}
	if len(args) > 1 {
		now, _ = args[1].(time.Time)
	}
	return buildOpenAICompactProbeExtraUpdatesLegacy(resp, body, probeErr, found, now)
}

func buildOpenAICompactProbeExtraUpdatesV2(resp *http.Response, body []byte, probeErr error, verdict openAIProbeVerdict, verdictReason string, startedAt, now time.Time) map[string]any {
	updates := map[string]any{
		openAICompactProbeLastStatusExtraKey:         nil,
		OpenAICompactProbeObservedAtUnixNanoExtraKey: startedAt.UTC().UnixNano(),
	}
	if resp != nil {
		updates[openAICompactProbeLastStatusExtraKey] = resp.StatusCode
	}
	switch {
	case probeErr != nil:
		errMsg := verdictReason
		if errMsg == "" {
			errMsg = probeErr.Error()
		}
		updates[openAICompactProbeLastErrorExtraKey] = truncateString(sanitizeUpstreamErrorMessage(errMsg), 2048)
	case resp == nil:
		updates[openAICompactProbeLastErrorExtraKey] = "compact probe failed"
	default:
		errMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
		if errMsg == "" && len(body) > 0 {
			errMsg = strings.TrimSpace(string(body))
		}
		if errMsg == "" && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			errMsg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		if verdictReason != "" {
			errMsg = verdictReason
		}
		errMsg = truncateString(sanitizeUpstreamErrorMessage(errMsg), 2048)
		switch verdict {
		case openAIProbeVerdictSupported:
			updates[openAICompactProbeSupportedExtraKey] = true
			updates[openAICompactProbeVersionExtraKey] = openAICompactProbeProtocolVersion
			updates[openAICompactProbeCheckedAtExtraKey] = now.UTC().Format(time.RFC3339Nano)
			updates[openAICompactProbeLastErrorExtraKey] = ""
		case openAIProbeVerdictUnsupported:
			updates[openAICompactProbeSupportedExtraKey] = false
			updates[openAICompactProbeVersionExtraKey] = openAICompactProbeProtocolVersion
			updates[openAICompactProbeCheckedAtExtraKey] = now.UTC().Format(time.RFC3339Nano)
			updates[openAICompactProbeLastErrorExtraKey] = errMsg
		default:
			updates[openAICompactProbeLastErrorExtraKey] = errMsg
		}
	}
	return updates
}

func buildOpenAICompactProbeExtraUpdatesLegacy(resp *http.Response, body []byte, probeErr error, compactionFound bool, now time.Time) map[string]any {
	updates := map[string]any{
		openAICompactProbeCheckedAtExtraKey:  now.Format(time.RFC3339),
		openAICompactProbeLastStatusExtraKey: nil,
	}
	if resp != nil {
		updates[openAICompactProbeLastStatusExtraKey] = resp.StatusCode
	}
	switch {
	case probeErr != nil:
		updates[openAICompactProbeLastErrorExtraKey] = truncateString(sanitizeUpstreamErrorMessage(probeErr.Error()), 2048)
	case resp == nil:
		updates[openAICompactProbeLastErrorExtraKey] = "compact probe failed"
	default:
		errMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
		if errMsg == "" && len(body) > 0 {
			errMsg = strings.TrimSpace(string(body))
		}
		if errMsg == "" && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			errMsg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		errMsg = truncateString(sanitizeUpstreamErrorMessage(errMsg), 2048)
		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300 && compactionFound:
			updates[openAICompactProbeSupportedExtraKey] = true
			updates[openAICompactProbeLastErrorExtraKey] = ""
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			updates[openAICompactProbeSupportedExtraKey] = false
			updates[openAICompactProbeLastErrorExtraKey] = "upstream returned 2xx without a compaction output item (native remote compaction v2 unsupported)"
		default:
			if shouldMarkOpenAICompactUnsupported(resp.StatusCode, body) {
				updates[openAICompactProbeSupportedExtraKey] = false
			}
			updates[openAICompactProbeLastErrorExtraKey] = errMsg
		}
	}
	return updates
}

func mergeExtraUpdates(base map[string]any, more map[string]any) map[string]any {
	if len(base) == 0 && len(more) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(more))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range more {
		out[key] = value
	}
	return out
}

func compactProbeSessionID(accountID int64) string {
	seed := "anonymous"
	if accountID > 0 {
		seed = strconv.FormatInt(accountID, 10)
	}
	return deriveStableUUIDv4("sub2api:codex-compact-probe:v1:" + seed)
}
