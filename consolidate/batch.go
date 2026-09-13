// cogmem - Cognitive Memory
// License: MIT

package consolidate

// BatchOptions bounds one consolidation input.
type BatchOptions struct {
	MaxMessages     int // hard count cap (default 200)
	MaxInputTokens  int // approx input token budget (default 96000)
	PerMessageChars int // truncate any single message to this many chars (default 12000)
	OverheadTokens  int // estimated tokens already used by prompt + curated + current_state
}

// DefaultBatchOptions returns the documented defaults.
func DefaultBatchOptions() BatchOptions {
	return BatchOptions{
		MaxMessages:     defaultMaxMessages,
		MaxInputTokens:  defaultMaxInputTokens,
		PerMessageChars: defaultPerMessageChars,
	}
}

func (o BatchOptions) withDefaults() BatchOptions {
	if o.MaxMessages <= 0 {
		o.MaxMessages = defaultMaxMessages
	}
	if o.MaxInputTokens <= 0 {
		o.MaxInputTokens = defaultMaxInputTokens
	}
	if o.PerMessageChars <= 0 {
		o.PerMessageChars = defaultPerMessageChars
	}
	return o
}

// EstimateTokens approximates token count without a tokenizer dependency. ~4
// chars per token is a conservative English-text rule of thumb.
func EstimateTokens(s string) int { return (len(s) + approxCharsPerToken - 1) / approxCharsPerToken }

// TruncateText caps s at maxChars, appending a marker if it cut anything.
func TruncateText(s string, maxChars int) (string, bool) {
	if maxChars <= 0 || len(s) <= maxChars {
		return s, false
	}
	return s[:maxChars] + " …[truncated]", true
}

// SelectBatch takes messages in ascending seq order and returns the prefix that
// fits within the count and token budgets (per-message text truncated in place).
// lastSeq is the seq of the final included message (advance the watermark to it);
// more reports whether unprocessed messages remain after this batch (cold-start
// backlog is then handled by subsequent runs). At least one message is always
// returned if any are given, so progress is guaranteed even for an oversized
// single message.
func SelectBatch(msgs []Message, opt BatchOptions) (batch []Message, lastSeq int64, more bool) {
	opt = opt.withDefaults()
	budget := opt.MaxInputTokens - opt.OverheadTokens
	if budget < 0 {
		budget = 0
	}
	for i := range msgs {
		if len(batch) >= opt.MaxMessages {
			more = true
			break
		}
		m := msgs[i]
		m.Text, _ = TruncateText(m.Text, opt.PerMessageChars)
		tok := EstimateTokens(m.Text) + 8 // small per-message framing overhead
		if len(batch) > 0 && tok > budget {
			more = true
			break
		}
		batch = append(batch, m)
		lastSeq = m.Seq
		budget -= tok
	}
	if !more && len(batch) < len(msgs) {
		more = true
	}
	return batch, lastSeq, more
}

// MeaningfulRole reports whether a message role carries consolidatable content.
// Tool-call/tool-result plumbing is excluded (it is also what the message
// trigger counts).
func MeaningfulRole(role string) bool {
	switch role {
	case "user", "assistant":
		return true
	default:
		return false
	}
}
