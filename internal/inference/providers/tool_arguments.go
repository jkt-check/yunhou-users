package providers

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
)

var errToolArgumentObject = errors.New("providers: Messages tool arguments must be a complete JSON object")

func toolArgumentObject(args string) (json.RawMessage, error) {
	raw := json.RawMessage(args)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(raw) {
		return nil, errToolArgumentObject
	}
	return raw, nil
}

func validateMessagesCompletion(v *completionView) error {
	if len(v.Choices) != 1 {
		return errors.New("providers: Messages translation requires exactly one completion choice")
	}
	for _, call := range v.Choices[0].Message.ToolCalls {
		if _, err := toolArgumentObject(call.Function.Arguments); err != nil {
			return err
		}
	}
	return nil
}

// Messages has one response, not a collection of alternative choices.
// Reject alternatives before their tool indexes can collide.
type messagesToolArguments struct {
	calls map[int]*strings.Builder
	err   error
}

func (a *messagesToolArguments) observe(ev pumpEvent) error {
	if a.err != nil {
		return a.err
	}
	if ev.done {
		return a.validate()
	}
	if ev.chunk == nil {
		return nil
	}
	if len(ev.chunk.Choices) > 1 {
		a.err = errors.New("providers: Messages cannot translate multiple completion choices")
		return a.err
	}
	for _, choice := range ev.chunk.Choices {
		if choice.Index != 0 {
			a.err = errors.New("providers: Messages cannot translate alternative completion choices")
			return a.err
		}
		for _, call := range choice.Delta.ToolCalls {
			if a.calls == nil {
				a.calls = make(map[int]*strings.Builder)
			}
			buf := a.calls[call.Index]
			if buf == nil {
				buf = &strings.Builder{}
				a.calls[call.Index] = buf
			}
			buf.WriteString(call.Function.Arguments)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if err := a.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *messagesToolArguments) validate() error {
	for _, buf := range a.calls {
		if _, err := toolArgumentObject(buf.String()); err != nil {
			a.err = err
			return err
		}
	}
	return nil
}

// Native taps meter before their normalized DONE reaches the reader.
// Gate completion on our own observed DONE, but keep draining rejected
// output so late usage and real upstream failures remain authoritative.
type messagesValidationTap struct {
	mu   sync.Mutex
	base UsageTap
	pump chunkPump
	args messagesToolArguments
	done bool
}

func (t *messagesValidationTap) Feed(p []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.base.Feed(p); err != nil {
		return err
	}
	events, err := t.pump.Feed(p)
	for _, ev := range events {
		if ev.done {
			t.done = true
		}
		_ = t.args.observe(ev)
	}
	return err
}

func (t *messagesValidationTap) Result() TapResult {
	t.mu.Lock()
	defer t.mu.Unlock()
	res := t.base.Result()
	if t.pump.err != nil {
		res.Terminal, res.RejectedComplete = false, false
	} else if t.args.err != nil {
		res.RejectedComplete = t.done && (res.Terminal || res.RejectedComplete)
		res.Terminal = false
	} else {
		res.Terminal = res.Terminal && t.done
	}
	return res
}
