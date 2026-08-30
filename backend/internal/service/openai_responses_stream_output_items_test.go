package service

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A terminal event that arrives with an empty output must be rebuilt from the
// items the stream reported, not from delta accumulation. The accumulator
// models only one reasoning and one message, so rebuilding through it collapses
// a multi-item turn into a single fabricated message.
func TestNormalizeResponsesStreamingTerminalOutputPreservesReportedItems(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()

	doneItems.Observe([]byte(`{
		"type":"response.output_item.done",
		"output_index":0,
		"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"opaque"}
	}`))
	doneItems.Observe([]byte(`{
		"type":"response.output_item.done",
		"output_index":1,
		"item":{"id":"msg_1","type":"message","status":"completed","phase":"final_answer","role":"assistant","content":[{"type":"output_text","text":"shipped","annotations":[],"logprobs":[]}]}
	}`))

	normalized, changed := normalizeResponsesStreamingTerminalOutput(
		[]byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`),
		nil,
		doneItems,
		nil,
	)
	require.True(t, changed)

	output := gjson.GetBytes(normalized, "response.output")
	require.True(t, output.IsArray())
	require.Len(t, output.Array(), 2, "both reported items must survive")

	require.Equal(t, "reasoning", gjson.GetBytes(normalized, "response.output.0.type").String())
	require.Equal(t, "rs_1", gjson.GetBytes(normalized, "response.output.0.id").String())
	require.Equal(t, "opaque", gjson.GetBytes(normalized, "response.output.0.encrypted_content").String(),
		"fields the gateway does not model must survive verbatim")

	require.Equal(t, "message", gjson.GetBytes(normalized, "response.output.1.type").String())
	require.Equal(t, "msg_1", gjson.GetBytes(normalized, "response.output.1.id").String(),
		"the reported id must be reused, not regenerated")
	require.Equal(t, "completed", gjson.GetBytes(normalized, "response.output.1.status").String())
	require.Equal(t, "final_answer", gjson.GetBytes(normalized, "response.output.1.phase").String())
	require.Equal(t, "shipped", gjson.GetBytes(normalized, "response.output.1.content.0.text").String())
}

// Items are ordered by output_index, not by arrival order.
func TestResponsesStreamOutputItemsOrderByOutputIndex(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{"type":"response.output_item.done","output_index":2,"item":{"id":"c","type":"message"}}`))
	doneItems.Observe([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"a","type":"reasoning"}}`))

	built, ok := doneItems.BuildOutput()
	require.True(t, ok)
	require.Equal(t, "a", gjson.GetBytes(built, "0.id").String())
	require.Equal(t, "c", gjson.GetBytes(built, "1.id").String())
}

// A stream that never reports a done item keeps the previous rebuild path.
func TestNormalizeResponsesStreamingTerminalOutputIgnoresNonDoneEvents(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`))
	doneItems.Observe([]byte(`{"type":"response.output_text.delta","output_index":0,"delta":"hi"}`))
	require.False(t, doneItems.HasItems())

	raw := []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)
	normalized, changed := normalizeResponsesStreamingTerminalOutput(raw, nil, doneItems, nil)
	require.False(t, changed)
	require.Equal(t, string(raw), string(normalized))
}

// The terminal event can arrive with a non-empty but truncated output: the
// stream reported two items, the terminal carries one, and its id was not the
// one the stream reported. The reported items win.
func TestNormalizeResponsesStreamingTerminalOutputRepairsTruncatedOutput(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"id":"rs_real","type":"reasoning","status":"in_progress","summary":[]}
	}`))
	doneItems.Observe([]byte(`{
		"type":"response.output_item.done","output_index":1,
		"item":{"id":"msg_real","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"shipped","annotations":[],"logprobs":[]}]}
	}`))

	normalized, changed := normalizeResponsesStreamingTerminalOutput([]byte(`{
		"type":"response.completed",
		"response":{"status":"completed","output":[{"type":"message","role":"assistant","id":"msg_fabricated","status":"completed","content":[{"type":"output_text","text":"shipped","annotations":[],"logprobs":[]}]}]}
	}`), nil, doneItems, nil)
	require.True(t, changed)

	require.Len(t, gjson.GetBytes(normalized, "response.output").Array(), 2)
	require.Equal(t, "reasoning", gjson.GetBytes(normalized, "response.output.0.type").String())
	require.Equal(t, "rs_real", gjson.GetBytes(normalized, "response.output.0.id").String())
	require.Equal(t, "msg_real", gjson.GetBytes(normalized, "response.output.1.id").String(),
		"the id the stream reported must replace the fabricated one")
}

// A terminal output that is already complete is never rewritten.
func TestNormalizeResponsesStreamingTerminalOutputLeavesCompleteOutputAlone(t *testing.T) {
	doneItems := newResponsesStreamOutputItems()
	doneItems.Observe([]byte(`{
		"type":"response.output_item.done","output_index":0,
		"item":{"id":"msg_real","type":"message","status":"completed"}
	}`))

	raw := []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","id":"msg_upstream","status":"completed","vendor":"keep"}]}}`)
	normalized, changed := normalizeResponsesStreamingTerminalOutput(raw, nil, doneItems, nil)
	require.False(t, changed)
	require.Equal(t, string(raw), string(normalized))
}

func TestResponsesDoneOutputItemsMergeSparseOutOfOrder(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":2,"item":{"id":"done-2","type":"function_call"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"done-0","type":"reasoning"}}`), "response.output_item.done")

	output := gjson.Parse(`[{"id":"slim-0","type":"reasoning"},{"id":"terminal-1","type":"message"}]`)
	merged, ok := items.MergeTerminalOutput(output, nil)
	require.True(t, ok)
	require.Equal(t, "done-0", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "terminal-1", gjson.GetBytes(merged, "1.id").String())
	require.Equal(t, "done-2", gjson.GetBytes(merged, "2.id").String())
}

func TestResponsesDoneOutputItemsMergeMatchesTerminalIdentityBeforeIndex(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"call-search","type":"tool_search_call","status":"completed"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":1,"item":{"id":"call-function","type":"function_call","status":"completed"}}`), "response.output_item.done")

	output := gjson.Parse(`[{"id":"call-function","type":"function_call","status":"in_progress"},{"id":"call-search","type":"tool_search_call","status":"in_progress"}]`)
	merged, ok := items.MergeTerminalOutput(output, nil)
	require.True(t, ok)
	require.Equal(t, "call-function", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "function_call", gjson.GetBytes(merged, "0.type").String())
	// Terminal fields are authoritative when the identities match.
	require.Equal(t, "in_progress", gjson.GetBytes(merged, "0.status").String())
	require.Equal(t, "call-search", gjson.GetBytes(merged, "1.id").String())
	require.Equal(t, "tool_search_call", gjson.GetBytes(merged, "1.type").String())
	require.Equal(t, "in_progress", gjson.GetBytes(merged, "1.status").String())
}

func TestResponsesDoneOutputItemsMergeRetainsDeltaFallback(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"compact-0","type":"compaction","encrypted_content":"opaque"}}`), "response.output_item.done")
	acc := apicompat.NewBufferedResponseAccumulator()
	acc.ProcessEvent(&apicompat.ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "fallback text"})

	normalized, ok := normalizeResponsesStreamingTerminalOutput(
		[]byte(`{"type":"response.completed","response":{"output":[]}}`),
		acc,
		items,
		nil,
	)
	require.True(t, ok)
	require.Equal(t, "compact-0", gjson.GetBytes(normalized, "response.output.0.id").String())
	require.Equal(t, "fallback text", gjson.GetBytes(normalized, "response.output.1.content.0.text").String())
}

func TestResponsesDoneOutputItemsRejectsMalformedIndicesAndAssignsMissing(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":-1,"item":{"id":"negative"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":0.5,"item":{"id":"fractional"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":"0","item":{"id":"string"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"item":{"id":"missing-0"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"item":{"id":"missing-1"}}`), "response.output_item.done")

	require.Empty(t, items.byIndex)
	require.Equal(t, []string{"missing-0", "missing-1"}, []string{
		items.withoutIndex[0].identity[len("id:"):],
		items.withoutIndex[1].identity[len("id:"):],
	})
}

func TestResponsesDoneOutputItemsRejectsHugeIndexWithoutGrowingOutput(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":999999999999999999999999,"item":{"id":"huge"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":4097,"item":{"id":"over-limit"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"terminal"}]`), nil)
	require.False(t, ok)
	require.Nil(t, merged)
	require.Empty(t, items.byIndex)
}

func TestResponsesDoneOutputItemsKeepsMissingItemWhenExplicitIndexArrives(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"item":{"id":"missing"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"explicit"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), nil)
	require.True(t, ok)
	require.Equal(t, "explicit", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "missing", gjson.GetBytes(merged, "1.id").String())
}

func TestResponsesDoneOutputItemsMissingIndexAlreadyInTerminalOutput(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"item":{"id":"msg-1","type":"message","status":"completed"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"msg-1","type":"message"}]`), nil)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "completed", gjson.GetBytes(merged, "0.status").String())
}

func TestResponsesDoneOutputItemsTerminalFieldsWinWhileDoneOnlyFieldsAreAdded(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"msg-1","type":"message","status":"completed","encrypted_content":"opaque"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"msg-1","type":"message","status":"in_progress","content":[{"type":"output_text","text":"terminal"}]}]`), nil)
	require.True(t, ok)
	require.Equal(t, "in_progress", gjson.GetBytes(merged, "0.status").String())
	require.Equal(t, "terminal", gjson.GetBytes(merged, "0.content.0.text").String())
	require.Equal(t, "opaque", gjson.GetBytes(merged, "0.encrypted_content").String())
}

func TestResponsesDoneOutputItemsDuplicateIdentityAtDifferentIndices(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"fc-1","type":"function_call","arguments":"old"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":4,"item":{"id":"fc-1","type":"function_call","arguments":"final"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), nil)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "final", gjson.GetBytes(merged, "0.arguments").String())
}

func TestResponsesDoneOutputItemsCachesIdentityAndMaintainsLookupIndex(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":3,"item":{"id":"fc-1","type":"function_call"}}`), "response.output_item.done")

	entry := items.byIndex[3]
	require.Equal(t, "id:fc-1", entry.identity)
	require.Same(t, entry, items.byIdentity["id:fc-1"])
}

func TestResponsesDoneOutputItemsRetainsMultipleSameTypeFallbackItems(t *testing.T) {
	items := newResponsesDoneOutputItems()
	fallback := []byte(`[{"type":"message","content":[{"type":"output_text","text":"one"}]},{"type":"message","content":[{"type":"output_text","text":"two"}]}]`)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 2)
	require.Equal(t, "one", gjson.GetBytes(merged, "0.content.0.text").String())
	require.Equal(t, "two", gjson.GetBytes(merged, "1.content.0.text").String())
}

func TestResponsesDoneOutputItemsSparseIndicesDoNotOverwriteFallbackItems(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":3,"item":{"id":"fc-done","type":"function_call","call_id":"call-1"}}`), "response.output_item.done")
	fallback := []byte(`[{"id":"reasoning-fallback","type":"reasoning"},{"id":"message-fallback","type":"message"},{"id":"fc-fallback","type":"function_call","call_id":"call-2"}]`)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.JSONEq(t, `[{"id":"reasoning-fallback","type":"reasoning"},{"id":"message-fallback","type":"message"},{"id":"fc-fallback","type":"function_call","call_id":"call-2"},{"id":"fc-done","type":"function_call","call_id":"call-1"}]`, string(merged))
}

func TestResponsesDoneMessageSuppressesTextDeltaFallback(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}}`), "response.output_item.done")
	acc := apicompat.NewBufferedResponseAccumulator()
	acc.ProcessEvent(&apicompat.ResponsesStreamEvent{Type: "response.output_text.delta", Delta: "final"})
	fallback, ok := buildResponsesOutputJSON(acc, nil)
	require.True(t, ok)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "final", gjson.GetBytes(merged, "0.content.0.text").String())
}

func TestResponsesDoneReasoningSuppressesReasoningDeltaFallback(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}}`), "response.output_item.done")
	acc := apicompat.NewBufferedResponseAccumulator()
	acc.ProcessEvent(&apicompat.ResponsesStreamEvent{Type: "response.reasoning_summary_text.delta", Delta: "thought"})
	fallback, ok := buildResponsesOutputJSON(acc, nil)
	require.True(t, ok)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "thought", gjson.GetBytes(merged, "0.summary.0.text").String())
}

func TestResponsesDoneImageSuppressesIDLessImageFallback(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"type":"image_generation_call","status":"completed","output_format":"png","result":"image-data"}}`), "response.output_item.done")
	fallback := []byte(`[{"type":"image_generation_call","status":"completed","output_format":"png","result":"image-data"}]`)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "image-data", gjson.GetBytes(merged, "0.result").String())
}

func TestResponsesDoneOutputItemsNilWithFallback(t *testing.T) {
	fallback := []byte(`[{"type":"message","content":[{"type":"output_text","text":"fallback"}]}]`)

	merged, ok := (*responsesDoneOutputItems)(nil).MergeTerminalOutput(gjson.Parse(`[]`), fallback)
	require.True(t, ok)
	require.JSONEq(t, string(fallback), string(merged))
}

func TestResponsesDoneOutputItemsOverflowFallsBackToTerminalOutput(t *testing.T) {
	items := newResponsesDoneOutputItems()
	for index := 0; index <= maxResponsesDoneOutputItems; index++ {
		items.ProcessEvent([]byte(fmt.Sprintf(`{"item":{"id":"item-%d"}}`, index)), "response.output_item.done")
	}

	require.True(t, items.overflowed)
	require.False(t, items.HasContent())
	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"terminal","type":"message"}]`), nil)
	require.False(t, ok)
	require.Nil(t, merged)
}

func TestResponsesDoneOutputItemsSparseDoneOnlyOutputHasNoNulls(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":1000,"item":{"id":"done-1000"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":2,"item":{"id":"done-2"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[]`), nil)
	require.True(t, ok)
	require.JSONEq(t, `[{"id":"done-2"},{"id":"done-1000"}]`, string(merged))
	require.NotContains(t, string(merged), "null")
}

func TestResponsesDoneOutputItemsFinalIdentityDedupKeepsFirstPositionAndLatestItem(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"item":{"id":"shared","type":"message","status":"completed"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(
		gjson.Parse(`[{"id":"shared","type":"message","status":"in_progress"},{"id":"other","type":"reasoning"}]`),
		nil,
	)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 2)
	require.Equal(t, "shared", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "completed", gjson.GetBytes(merged, "0.status").String())
	require.Equal(t, "other", gjson.GetBytes(merged, "1.id").String())
}

func TestResponsesDoneOutputItemsSkippedCompleteSlotSuppressesDuplicateFallback(t *testing.T) {
	items := newResponsesDoneOutputItems()
	// The terminal item is already complete but has a provider-generated id that
	// differs from the done event. The done event must not be appended a second
	// time when the delta accumulator produces the same semantic message.
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"done-id","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"same"}]}}`), "response.output_item.done")
	fallback := []byte(`[{"id":"fallback-id","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"same"}]}]`)

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"terminal-id","type":"message","role":"assistant","status":"completed","vendor":"keep","content":[{"type":"output_text","text":"same"}]}]`), fallback)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	require.Equal(t, "terminal-id", gjson.GetBytes(merged, "0.id").String())
}

func TestResponsesDoneOutputItemsRepairRefreshesTerminalIdentityIndex(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"new-id","type":"message","status":"completed"}}`), "response.output_item.done")
	items.ProcessEvent([]byte(`{"output_index":1,"item":{"id":"old-id","type":"reasoning","status":"completed"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(
		gjson.Parse(`[{"id":"old-id","type":"message","status":"in_progress"}]`),
		nil,
	)
	require.True(t, ok)
	require.Equal(t, "new-id", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "old-id", gjson.GetBytes(merged, "1.id").String())
}

func TestResponsesDoneOutputItemsRepairsMissingTerminalIDEvenWhenCompleted(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"done-id","type":"message","status":"completed","content":[{"type":"output_text","text":"answer"}]}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]`), nil)
	require.True(t, ok)
	require.Equal(t, "done-id", gjson.GetBytes(merged, "0.id").String())
}

func TestResponsesDoneOutputItemsMatchesCallIDWhenProviderRegeneratesItemID(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"done-id","call_id":"call-stable","type":"function_call","status":"completed","arguments":"{}"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[{"id":"terminal-id","call_id":"call-stable","type":"function_call","status":"in_progress"}]`), nil)
	require.True(t, ok)
	require.Len(t, gjson.ParseBytes(merged).Array(), 1)
	// The terminal id/status remain authoritative, while done-only arguments
	// are recovered through the stable call_id identity.
	require.Equal(t, "terminal-id", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "call-stable", gjson.GetBytes(merged, "0.call_id").String())
	require.Equal(t, "in_progress", gjson.GetBytes(merged, "0.status").String())
	require.Equal(t, "{}", gjson.GetBytes(merged, "0.arguments").String())
}

func TestResponsesDoneOutputItemsPrefersExactIDOverCallIDAlias(t *testing.T) {
	items := newResponsesDoneOutputItems()
	items.ProcessEvent([]byte(`{"output_index":0,"item":{"id":"exact-id","call_id":"call-shared","type":"function_call","status":"completed","arguments":"done"}}`), "response.output_item.done")

	merged, ok := items.MergeTerminalOutput(gjson.Parse(`[
		{"id":"exact-id","call_id":"call-other","type":"function_call","status":"in_progress","arguments":"terminal-exact"},
		{"id":"other-id","call_id":"call-shared","type":"function_call","status":"in_progress","arguments":"terminal-alias"}
	]`), nil)
	require.True(t, ok)
	// The exact item id wins even though the done item's call_id aliases a
	// different terminal item; the second terminal item remains untouched.
	require.Equal(t, "exact-id", gjson.GetBytes(merged, "0.id").String())
	require.Equal(t, "terminal-exact", gjson.GetBytes(merged, "0.arguments").String())
	require.Equal(t, "other-id", gjson.GetBytes(merged, "1.id").String())
	require.Equal(t, "terminal-alias", gjson.GetBytes(merged, "1.arguments").String())
}
