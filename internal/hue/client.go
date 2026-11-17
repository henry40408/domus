// Package hue is the Philips Hue Bridge integration over the CLIP v2 API.
//
// The bridge serves HTTPS with a certificate signed by a private CA, so
// certificate verification is disabled for bridge connections only (LAN
// traffic to a fixed address).
package hue

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Timing holds the durations of the integration. A zero field means the
// production value, so tests only set what they shorten.
type Timing struct {
	// HTTPTimeout is the total time of one request to the bridge.
	HTTPTimeout time.Duration
	// ConnectTimeout is the time to open a connection.
	ConnectTimeout time.Duration
	// StreamIdle is how long the event stream may stay silent before it is
	// dropped.
	StreamIdle time.Duration
	// ReconnectDelay is the fixed wait before reconnecting the event stream.
	ReconnectDelay time.Duration
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

func (t Timing) withDefaults() Timing {
	return Timing{
		HTTPTimeout:    orDefault(t.HTTPTimeout, 10*time.Second),
		ConnectTimeout: orDefault(t.ConnectTimeout, 10*time.Second),
		StreamIdle:     orDefault(t.StreamIdle, 120*time.Second),
		ReconnectDelay: orDefault(t.ReconnectDelay, 5*time.Second),
	}
}

// BridgeBase is the base URL for a bridge address. ip is a bare host or IP; a
// full http(s):// base is passed through (used by tests).
func BridgeBase(ip string) string {
	if strings.HasPrefix(ip, "http://") || strings.HasPrefix(ip, "https://") {
		return strings.TrimRight(ip, "/")
	}
	return "https://" + ip
}

// newBridgeHTTP is the client for bridge connections. timeout is the total
// request time; zero means none (event streams).
func newBridgeHTTP(timeout, connectTimeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: connectTimeout}).DialContext,
		// The bridge's certificate is signed by a private CA and the bridge sits at
		// a fixed LAN address, so verification is off. Never reuse this transport
		// for anything else.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// Client talks CLIP v2 to one bridge.
type Client struct {
	base   string
	key    string
	logger *slog.Logger
	http   *http.Client
	sse    *http.Client
}

// NewClient is a client for the bridge at base using the application key. A nil
// logger means slog.Default().
func NewClient(base, key string, timing Timing, logger *slog.Logger) *Client {
	timing = timing.withDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		base:   strings.TrimRight(base, "/"),
		key:    key,
		logger: logger,
		http:   newBridgeHTTP(timing.HTTPTimeout, timing.ConnectTimeout),
		sse:    newBridgeHTTP(0, timing.ConnectTimeout),
	}
}

// Close drops the idle connections, which would keep goroutines alive.
func (c *Client) Close() {
	c.http.CloseIdleConnections()
	c.sse.CloseIdleConnections()
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("hue-application-key", c.key)
	return req, nil
}

// listResource is the data array of a resource type.
func (c *Client) listResource(ctx context.Context, kind string) ([]any, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/clip/v2/resource/"+kind, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("bridge returned %s", resp.Status)
	}
	var body any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return arrayAt(body, "data"), nil
}

// ListLights is every light the bridge knows.
func (c *Client) ListLights(ctx context.Context) ([]Light, error) {
	resources, err := c.listResource(ctx, "light")
	if err != nil {
		return nil, err
	}
	var lights []Light
	for _, resource := range resources {
		if light, ok := lightFromResource(resource); ok {
			lights = append(lights, light)
		}
	}
	return lights, nil
}

// ListScenes is the scenes and smart scenes with their room and zone names
// resolved.
func (c *Client) ListScenes(ctx context.Context) ([]Scene, error) {
	resources, err := c.listResource(ctx, "scene")
	if err != nil {
		return nil, err
	}
	// Older bridges may not know smart scenes: keep the plain scenes then.
	smart, err := c.listResource(ctx, smartSceneType)
	if err != nil {
		c.logger.Warn("hue: smart scene sync failed", "error", err)
	}
	resources = append(resources, smart...)
	groups, err := c.groupNames(ctx)
	if err != nil {
		return nil, err
	}
	var scenes []Scene
	for _, resource := range resources {
		if scene, ok := sceneFromResource(resource, groups); ok {
			scenes = append(scenes, scene)
		}
	}
	return scenes, nil
}

// groupNames maps room and zone uuids to their names.
func (c *Client) groupNames(ctx context.Context) (map[string]string, error) {
	names := map[string]string{}
	for _, kind := range []string{"room", "zone"} {
		resources, err := c.listResource(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			id, okID := stringAt(resource, "id")
			name, okName := stringAt(resource, "metadata", "name")
			if okID && okName {
				names[id] = name
			}
		}
	}
	return names, nil
}

// ListRooms is the rooms and zones that have a grouped_light.
func (c *Client) ListRooms(ctx context.Context) ([]Room, error) {
	var rooms []Room
	for _, kind := range []string{"room", "zone"} {
		resources, err := c.listResource(ctx, kind)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if room, ok := roomFromResource(resource, kind); ok {
				rooms = append(rooms, room)
			}
		}
	}
	return rooms, nil
}

// ListConnectivity maps each zigbee_connectivity id to its device and whether
// the device is reachable.
func (c *Client) ListConnectivity(ctx context.Context) (map[string]link, error) {
	resources, err := c.listResource(ctx, "zigbee_connectivity")
	if err != nil {
		return nil, err
	}
	links := map[string]link{}
	for _, resource := range resources {
		if id, l, ok := connectivityOf(resource); ok {
			links[id] = l
		}
	}
	return links, nil
}

// RecallScene activates a scene or smart scene.
func (c *Client) RecallScene(ctx context.Context, id string, smart bool) error {
	kind, action := "scene", "active"
	if smart {
		kind, action = smartSceneType, "activate"
	}
	return c.putResource(ctx, kind, id, map[string]any{"recall": map[string]any{"action": action}})
}

// SetLight sends a body to one light.
func (c *Client) SetLight(ctx context.Context, id string, body map[string]any) error {
	return c.putResource(ctx, "light", id, body)
}

// SetGroupedLight sends a body to a room's or zone's grouped_light.
func (c *Client) SetGroupedLight(ctx context.Context, id string, body map[string]any) error {
	return c.putResource(ctx, "grouped_light", id, body)
}

func (c *Client) putResource(ctx context.Context, kind, id string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest(ctx, http.MethodPut, "/clip/v2/resource/"+kind+"/"+id, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return checkPut(resp)
}

// checkPut accepts a PUT only with a 2xx status and an empty errors list.
func checkPut(resp *http.Response) error {
	text, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("bridge returned %s", resp.Status)
	}
	var body any
	if json.Unmarshal(text, &body) != nil {
		return nil
	}
	if errs := arrayAt(body, "errors"); len(errs) > 0 {
		encoded, _ := json.Marshal(errs)
		return fmt.Errorf("bridge rejected request: %s", encoded)
	}
	return nil
}

// eventStream opens the event stream; the caller closes the body.
func (c *Client) eventStream(ctx context.Context) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/eventstream/clip/v2", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.sse.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("event stream returned %s", resp.Status)
	}
	return resp, nil
}

var errEventStreamClosed = errors.New("event stream closed")
var errEventStreamIdle = errors.New("event stream idle timeout")
