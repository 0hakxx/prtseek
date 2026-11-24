package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"iter"
	"strings"

	"github.com/Autumn-27/norma/llm"
)

type flashResponsesFrame struct {
	Type      string          `json:"type"`
	Index     *int            `json:"output_index"`
	ItemID    string          `json:"item_id"`
	Delta     string          `json:"delta"`
	Arguments string          `json:"arguments"`
	Item      json.RawMessage `json:"item"`
	Response  json.RawMessage `json:"response"`
	Error     json.RawMessage `json:"error"`
}
type flashResponseToolState struct {
	item      flashResponseItem
	arguments strings.Builder
	done      bool
}
type flashResponseStream struct {
	tools    map[int]*flashResponseToolState
	ids      map[string]int
	bytes    int
	thinking strings.Builder
}

func (s *flashResponseStream) checkSize(n int) error {
	s.bytes += n
	if s.bytes > flashResponsesLimit {
		return flashError("oversize_response", "Responses 스트림이 로컬 32MiB 제한을 초과했습니다")
	}
	return nil
}
func (s *flashResponseStream) toolFor(f flashResponsesFrame) (*flashResponseToolState, error) {
	if f.Index == nil || *f.Index < 0 || *f.Index > 4096 {
		return nil, flashError("invalid_tool_index", "Responses 도구 이벤트에 유효한 output_index 가 없습니다")
	}
	t := s.tools[*f.Index]
	if t == nil || f.ItemID == "" || f.ItemID != t.item.ID {
		return nil, flashError("invalid_tool_identity", "Responses 도구 이벤트의 item_id/output_index 가 일치하지 않습니다")
	}
	return t, nil
}

func (s *flashResponseStream) observe(f flashResponsesFrame) error {
	switch f.Type {
	case "response.output_item.added":
		var item flashResponseItem
		if json.Unmarshal(f.Item, &item) != nil {
			return flashError("invalid_item", "Responses item 을 파싱할 수 없습니다")
		}
		if item.Type != "function_call" {
			return nil
		}
		if f.Index == nil || *f.Index < 0 || *f.Index > 4096 || item.ID == "" || item.CallID == "" || item.Name == "" {
			return flashError("incomplete_tool", "Responses 도구 item 에 식별자가 없습니다")
		}
		if _, ok := s.ids[item.ID]; ok {
			return flashError("duplicate_tool_id", "Responses item id 가 중복되었습니다")
		}
		if s.tools[*f.Index] != nil {
			return flashError("invalid_tool_index", "Responses output_index 가 중복되었습니다")
		}
		t := &flashResponseToolState{item: item}
		t.arguments.WriteString(item.Arguments)
		s.tools[*f.Index] = t
		s.ids[item.ID] = *f.Index
		return s.checkSize(len(item.Arguments))
	case "response.function_call_arguments.delta":
		t, err := s.toolFor(f)
		if err != nil {
			return err
		}
		if t.done {
			return flashError("late_tool_delta", "이미 완료된 도구에 파라미터 조각이 또 들어왔습니다")
		}
		t.arguments.WriteString(f.Delta)
		return s.checkSize(len(f.Delta))
	case "response.function_call_arguments.done":
		t, err := s.toolFor(f)
		if err != nil {
			return err
		}
		if t.arguments.Len() > 0 && t.arguments.String() != f.Arguments {
			return flashError("changed_tool_arguments", "Responses 전체 파라미터가 스트림 조각과 일치하지 않습니다")
		}
		t.arguments.Reset()
		t.arguments.WriteString(f.Arguments)
		t.done = true
		return validateFlashTool(llm.ContentBlock{ID: t.item.CallID, Name: t.item.Name, Input: json.RawMessage(f.Arguments)})
	case "response.output_item.done":
		var item flashResponseItem
		if json.Unmarshal(f.Item, &item) != nil {
			return flashError("invalid_item", "Responses 완료 item 을 파싱할 수 없습니다")
		}
		if item.Type != "function_call" {
			return nil
		}
		f.ItemID = item.ID
		t, err := s.toolFor(f)
		if err != nil {
			return err
		}
		if item.CallID != t.item.CallID || item.Name != t.item.Name || (t.arguments.Len() > 0 && item.Arguments != t.arguments.String()) {
			return flashError("changed_tool_arguments", "Responses 완료 도구가 이미 수집한 조각과 일치하지 않습니다")
		}
		t.arguments.Reset()
		t.arguments.WriteString(item.Arguments)
		t.done = true
		return validateFlashTool(llm.ContentBlock{ID: item.CallID, Name: item.Name, Input: json.RawMessage(item.Arguments)})
	}
	return nil
}

func (s *flashResponseStream) validateFinal(raw []byte) error {
	var response flashResponseEnvelope
	if json.Unmarshal(raw, &response) != nil {
		return flashError("invalid_response", "Responses 최종 상태가 유효하지 않습니다")
	}
	seen := map[int]bool{}
	for i, rawItem := range response.Output {
		var item flashResponseItem
		if json.Unmarshal(rawItem, &item) != nil {
			return flashError("invalid_item", "Responses 최종 상태 item 이 유효하지 않습니다")
		}
		if item.Type != "function_call" {
			continue
		}
		prior := s.tools[i]
		if prior == nil {
			return flashError("missing_tool_item", "Responses 완료 결과에 시작되지 않은 도구가 있습니다")
		}
		if prior.item.ID != item.ID || prior.item.CallID != item.CallID || prior.item.Name != item.Name || (prior.arguments.Len() > 0 && prior.arguments.String() != item.Arguments) {
			return flashError("changed_tool_arguments", "Responses 최종 상태의 도구가 스트림의 도구와 일치하지 않습니다")
		}
		seen[i] = true
	}
	if len(seen) != len(s.tools) {
		return flashError("missing_tool_item", "Responses 최종 상태에 이미 시작된 도구가 빠져 있습니다")
	}
	return nil
}

// Reasoning text is displayed incrementally. Answer/tool events wait for the
// authoritative terminal object so its original reasoning item metadata can
// be attached before the SDK flushes the thinking block into the transcript.
func (p *flashResponsesProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		data, req, err := p.request(ctx, req, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		resp, err := p.do(ctx, data, true)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		state := &flashResponseStream{tools: map[int]*flashResponseToolState{}, ids: map[string]int{}}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), flashResponsesLimit)
		if !yield(llm.StreamEvent{Type: llm.SEMessageStart}, nil) {
			return
		}
		for {
			event, payload, err := readFlashResponsesFrame(scanner)
			if err != nil {
				if err == io.EOF {
					err = flashError("incomplete_stream", "Responses 최종 상태 이벤트를 받지 못했습니다. 도구는 실행하지 않았습니다")
				}
				yield(llm.StreamEvent{}, err)
				return
			}
			var frame flashResponsesFrame
			if json.Unmarshal([]byte(payload), &frame) != nil {
				yield(llm.StreamEvent{}, flashError("invalid_frame", "Responses SSE JSON 이 유효하지 않습니다"))
				return
			}
			if event != "" {
				if frame.Type != "" && frame.Type != event {
					yield(llm.StreamEvent{}, flashError("invalid_frame", "Responses SSE event 와 type 이 충돌합니다"))
					return
				}
				frame.Type = event
			}
			if err := state.observe(frame); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			switch frame.Type {
			case "response.reasoning_text.delta":
				if err := state.checkSize(len(frame.Delta)); err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
				state.thinking.WriteString(frame.Delta)
				if frame.Delta != "" && !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: frame.Delta}, nil) {
					return
				}
			case "response.failed", "error":
				if len(frame.Error) == 0 || string(frame.Error) == "null" {
					var e flashResponseEnvelope
					_ = json.Unmarshal(frame.Response, &e)
					frame.Error = e.Error
				}
				if len(frame.Error) == 0 || string(frame.Error) == "null" {
					frame.Error = json.RawMessage(payload) // named error events may carry code/message directly
				}
				yield(llm.StreamEvent{}, flashRemoteError(frame.Error))
				return
			case "response.completed", "response.incomplete":
				var final flashResponseEnvelope
				if json.Unmarshal(frame.Response, &final) != nil || (frame.Type == "response.completed" && final.Status != "completed") || (frame.Type == "response.incomplete" && final.Status != "incomplete") {
					yield(llm.StreamEvent{}, flashError("invalid_response", "Responses 최종 상태 이벤트가 상태와 일치하지 않습니다"))
					return
				}
				msg, stop, usage, err := decodeFlashResponse(frame.Response)
				if err == nil {
					err = state.validateFinal(frame.Response)
				}
				if err == nil {
					err = p.integrity.validateResponse(req, msg, stop)
				}
				if err != nil {
					yield(llm.StreamEvent{}, err)
					return
				}
				var thought strings.Builder
				var items []json.RawMessage
				for _, b := range msg.Content {
					if b.Type == llm.BlockThinking {
						thought.WriteString(b.Thinking)
						raw, e := flashReplayReasoning(b)
						if e != nil {
							yield(llm.StreamEvent{}, e)
							return
						}
						items = append(items, raw...)
					}
				}
				full := thought.String()
				if !strings.HasPrefix(full, state.thinking.String()) {
					yield(llm.StreamEvent{}, flashError("changed_reasoning", "Responses 최종 상태의 사고가 스트림 조각과 일치하지 않습니다"))
					return
				}
				if tail := strings.TrimPrefix(full, state.thinking.String()); tail != "" {
					if !yield(llm.StreamEvent{Type: llm.SEThinkingDelta, Text: tail}, nil) {
						return
					}
				}
				if full != "" {
					if !yield(llm.StreamEvent{Type: llm.SEThinkingSignature, Text: flashEncodeReasoning(full, items)}, nil) {
						return
					}
				}
				p.integrity.remember(ctx, msg)
				for _, b := range msg.Content {
					switch b.Type {
					case llm.BlockText:
						if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text}, nil) {
							return
						}
					case llm.BlockToolUse:
						if !yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, nil) {
							return
						}
						if !yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)}, nil) {
							return
						}
					}
				}
				if !yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage}, nil) {
					return
				}
				yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
				return
			}
		}
	}
}

func readFlashResponsesFrame(scanner *bufio.Scanner) (string, string, error) {
	var event string
	var data strings.Builder
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if data.Len() > 0 {
				return event, strings.TrimSuffix(data.String(), "\n"), nil
			}
			event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteByte('\n')
			if data.Len() > flashResponsesLimit {
				return "", "", flashError("oversize_frame", "Responses SSE 프레임이 32MiB 를 초과했습니다")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}
	if data.Len() > 0 {
		return event, strings.TrimSuffix(data.String(), "\n"), nil
	}
	return "", "", io.EOF
}
