package backends

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"time"
)

// streamLGBody handles LangGraph requests where the caller has pre-built the
// complete run envelope in LGBody. The daemon injects resolved file attachments
// into the message content and forwards the request.
func streamLGBody(ctx context.Context, conn net.Conn, handoff HandoffData, p *langgraphProfile) (int64, error) {
	backendStart := time.Now()

	// Set content format from profile for attachment serialization
	handoff.ContentFormat = p.contentFormat

	// Inject resolved file attachments into the body (passthrough if none).
	body, err := mergeAttachmentsIntoLGBody(handoff.LGBody, handoff.ResolvedAttachments, handoff.ResolvedImages, p.contentFormat)
	if err != nil {
		return 0, fmt.Errorf("merge attachments: %w", err)
	}

	reqURL := langgraphRunURL(p, handoff.ThreadID)

	if slog.Default().Enabled(ctx, slog.LevelDebug) {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "", "  ") == nil {
			slog.Debug("langgraph v2 request body", "url", reqURL, "body", pretty.String())
		}
	}

	// Thread creation is lazy (only on 404); see doLangGraphRun.
	resp, err := doLangGraphRun(ctx, p, reqURL, body, handoff)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	return proxySSE(ctx, conn, resp.Body, "langgraph", backendStart)
}

// mergeAttachmentsIntoLGBody injects resolved file attachments into the last
// message of lg_body.input.messages. If there are no attachments, the body is
// returned unchanged with no allocations.
func mergeAttachmentsIntoLGBody(lgBody json.RawMessage, resolved map[string]ResolvedAttachment, images []ImageData, contentFormat string) ([]byte, error) {
	if len(resolved) == 0 && len(images) == 0 {
		return lgBody, nil
	}

	// Parse body → input → messages
	var body map[string]json.RawMessage
	if err := json.Unmarshal(lgBody, &body); err != nil {
		return nil, fmt.Errorf("parse lg_body: %w", err)
	}

	inputRaw, ok := body["input"]
	if !ok {
		slog.Warn("lg_body has no input field, skipping attachment injection")
		return lgBody, nil
	}

	var input map[string]json.RawMessage
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return nil, fmt.Errorf("parse lg_body.input: %w", err)
	}

	messagesRaw, ok := input["messages"]
	if !ok {
		slog.Warn("lg_body.input has no messages field, skipping attachment injection")
		return lgBody, nil
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(messagesRaw, &messages); err != nil {
		return nil, fmt.Errorf("parse lg_body.input.messages: %w", err)
	}
	if len(messages) == 0 {
		return lgBody, nil
	}

	// Modify the last message's content field.
	var lastMsg map[string]json.RawMessage
	if err := json.Unmarshal(messages[len(messages)-1], &lastMsg); err != nil {
		return nil, fmt.Errorf("parse last message: %w", err)
	}

	contentRaw, ok := lastMsg["content"]
	if !ok {
		return lgBody, nil
	}

	var newContent []byte
	if len(contentRaw) > 0 && contentRaw[0] == '"' {
		// Content is a JSON string — extract it and run the standard attachment injector.
		var contentStr string
		if err := json.Unmarshal(contentRaw, &contentStr); err != nil {
			return nil, fmt.Errorf("parse message content: %w", err)
		}
		newContent = appendContentWithAttachments(nil, contentStr, resolved, images, contentFormat)
	} else if len(contentRaw) > 0 && contentRaw[0] == '[' {
		// Content is already an array (PHP pre-built multimodal). Prepend any
		// unreferenced binary attachments/images before the existing elements.
		newContent = prependAttachmentsToContentArray(contentRaw, resolved, images, contentFormat)
	} else {
		return lgBody, nil
	}

	// Re-marshal bottom-up.
	lastMsg["content"] = newContent
	newLastMsgBytes, err := json.Marshal(lastMsg)
	if err != nil {
		return nil, fmt.Errorf("re-marshal last message: %w", err)
	}
	messages[len(messages)-1] = newLastMsgBytes

	newMessagesBytes, err := json.Marshal(messages)
	if err != nil {
		return nil, fmt.Errorf("re-marshal messages: %w", err)
	}
	input["messages"] = newMessagesBytes

	newInputBytes, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("re-marshal input: %w", err)
	}
	body["input"] = newInputBytes

	return json.Marshal(body)
}

// prependAttachmentsToContentArray prepends unreferenced binary attachments and
// images to an existing JSON content array. Text attachments are appended as a
// trailing text part. Existing array elements are untouched.
func prependAttachmentsToContentArray(existingArray []byte, resolved map[string]ResolvedAttachment, images []ImageData, contentFormat string) []byte {
	if len(resolved) == 0 && len(images) == 0 {
		return existingArray
	}

	var prependParts []byte
	first := true

	appendBinaryPart := func(mimeType, base64Data string) {
		if !first {
			prependParts = append(prependParts, ',')
		}
		first = false
		if contentFormat == "anthropic" && !isImageMime(mimeType) {
			prependParts = append(prependParts, `{"type":"document","source":{"type":"base64","media_type":"`...)
			prependParts = appendJSONEscaped(prependParts, mimeType)
			prependParts = append(prependParts, `","data":"`...)
			prependParts = appendJSONEscaped(prependParts, base64Data)
			prependParts = append(prependParts, `"}}`...)
		} else {
			prependParts = append(prependParts, `{"type":"image_url","image_url":{"url":"data:`...)
			prependParts = appendJSONEscaped(prependParts, mimeType)
			prependParts = append(prependParts, `;base64,`...)
			prependParts = appendJSONEscaped(prependParts, base64Data)
			prependParts = append(prependParts, `"}}`...)
		}
	}

	for _, img := range images {
		mime := img.MimeType
		if mime == "" {
			mime = "image/jpeg"
		}
		appendBinaryPart(mime, img.Base64)
	}

	// Sort ref names for deterministic output (map iteration order is random).
	names := make([]string, 0, len(resolved))
	for name := range resolved {
		names = append(names, name)
	}
	sort.Strings(names)
	var textParts []byte
	for _, name := range names {
		att := resolved[name]
		if att.IsText {
			// Append text attachments as a trailing text part rather than silently dropping them.
			textParts = appendJSONEscaped(textParts, att.Text)
		} else {
			appendBinaryPart(att.MimeType, att.Base64)
		}
	}

	// Build result: '[' + prepend + [comma + inner if non-empty] + [trailing text part] + ']'
	// inner is the existing array's contents without the surrounding brackets and
	// without insignificant whitespace, so "[ ]" and "[\n]" are treated as empty
	// (otherwise a trailing comma would be emitted and the body would be invalid JSON).
	inner := bytes.TrimSpace(existingArray[1:])
	inner = bytes.TrimSpace(inner[:len(inner)-1]) // strip closing ']'
	arrayEmpty := len(inner) == 0

	var result []byte
	result = append(result, '[')
	result = append(result, prependParts...)

	if !arrayEmpty {
		if len(prependParts) > 0 {
			result = append(result, ',')
		}
		result = append(result, inner...)
	}

	if len(textParts) > 0 {
		if len(result) > 1 { // something already in the array
			result = append(result, ',')
		}
		result = append(result, `{"type":"text","text":"`...)
		result = append(result, textParts...)
		result = append(result, `"}`...)
	}

	result = append(result, ']')
	return result
}

func isImageMime(mimeType string) bool {
	return strings.HasPrefix(strings.ToLower(mimeType), "image/")
}
