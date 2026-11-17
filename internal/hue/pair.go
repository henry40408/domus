package hue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ErrLinkButtonNotPressed means the bridge refused to pair because its link
// button was not pressed.
var ErrLinkButtonNotPressed = errors.New("link button not pressed")

const (
	pairTimeout         = 10 * time.Second
	linkButtonErrorType = 101
)

// Pair asks the bridge at base for an application key. The bridge's link button
// must have been pressed.
func Pair(ctx context.Context, base string) (string, error) {
	client := newBridgeHTTP(pairTimeout, pairTimeout)
	defer client.CloseIdleConnections()
	payload, err := json.Marshal(map[string]any{"devicetype": "domus#server", "generateclientkey": true})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return pairResult(body)
}

// pairResult reads the first element of the bridge's answer.
func pairResult(body any) (string, error) {
	answers, _ := body.([]any)
	if len(answers) == 0 {
		return "", errors.New("empty response")
	}
	first := answers[0]
	if key, ok := stringAt(first, "success", "username"); ok {
		return key, nil
	}
	if code, ok := floatAt(first, "error", "type"); ok && code == linkButtonErrorType {
		return "", ErrLinkButtonNotPressed
	}
	encoded, _ := json.Marshal(first)
	return "", errors.New(string(encoded))
}
