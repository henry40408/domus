package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
)

// API seeds a server over its admin API, so a scenario starts from a known
// state instead of clicking through the setup again.
type API struct {
	server *Server
	client *http.Client
}

// API returns a client with its own cookie jar.
func (s *Server) API() *API {
	jar, _ := cookiejar.New(nil)
	return &API{server: s, client: &http.Client{Jar: jar}}
}

// Request is one call to /api/domus. A nil body sends none.
type Request struct {
	Method string
	Path   string
	Body   any
}

// Do sends the request and fails on any status other than 2xx. It returns the
// decoded JSON body (nil when empty).
func (a *API) Do(request Request) (any, error) {
	var body io.Reader
	if request.Body != nil {
		encoded, err := json.Marshal(request.Body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	httpRequest, err := http.NewRequest(request.Method, a.server.BaseURL+"/api/domus"+request.Path, body)
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s %s answered %d: %s", request.Method, request.Path, response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	return decoded, nil
}

// Admin is the account the seeded scenarios sign in with.
const (
	AdminUsername = "demo"
	AdminPassword = "demo-password-123"
)

// SetupAdmin creates the first admin with the setup code.
func (a *API) SetupAdmin() error {
	_, err := a.Do(Request{Method: "POST", Path: "/setup", Body: map[string]string{
		"username": AdminUsername, "password": AdminPassword, "setup_code": a.server.SetupCode,
	}})
	return err
}

// PairBridge pairs the server with the fake bridge.
func (a *API) PairBridge() error {
	_, err := a.Do(Request{Method: "POST", Path: "/hue/pair", Body: map[string]string{"ip": a.server.BridgeURL}})
	return err
}
