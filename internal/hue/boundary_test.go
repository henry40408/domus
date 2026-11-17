package hue

import (
	"net/http"
	"testing"
	"time"
)

// statusClient is a client whose bridge answers every request with status.
func statusClient(t *testing.T, status int) *Client {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errors":[],"data":[]}`))
	})
	client := NewClient(serve(t, handler), "key", Timing{}, quietLogger())
	t.Cleanup(client.Close)
	return client
}

func TestEveryBridgeCallTreats2xxAsSuccessAndNothingElse(t *testing.T) {
	checkGoroutines(t)
	calls := map[string]func(c *Client) error{
		"list": func(c *Client) error { _, err := c.ListLights(ctx); return err },
		"put":  func(c *Client) error { return c.SetLight(ctx, "id", J{"on": J{"on": true}}) },
		"stream": func(c *Client) error {
			resp, err := c.eventStream(ctx)
			if err == nil {
				_ = resp.Body.Close()
			}
			return err
		},
	}
	for name, call := range calls {
		for status, wantOK := range map[int]bool{200: true, 299: true, 300: false, 404: false} {
			err := call(statusClient(t, status))
			if (err == nil) != wantOK {
				t.Errorf("%s with status %d: err = %v, want success %v", name, status, err, wantOK)
			}
		}
	}
}

func TestUint16AtAcceptsWholeNumbersInTheRangeOnly(t *testing.T) {
	tests := []struct {
		value float64
		want  bool
	}{{0, true}, {1, true}, {65535, true}, {-1, false}, {65536, false}, {1.5, false}}
	for _, tt := range tests {
		got, ok := uint16At(J{"v": tt.value}, "v")
		if ok != tt.want {
			t.Errorf("uint16At(%v) ok = %v, want %v", tt.value, ok, tt.want)
		}
		if ok && float64(got) != tt.value {
			t.Errorf("uint16At(%v) = %d", tt.value, got)
		}
	}
}

func TestAnEmptyEventBlockDoesNotStallTheParser(t *testing.T) {
	var p sseParser
	sameJSON(t, p.push([]byte("\n\ndata: x\n\n")), []string{"x"})
}

func TestTimingDefaults(t *testing.T) {
	want := Timing{
		HTTPTimeout:    10 * time.Second,
		ConnectTimeout: 10 * time.Second,
		StreamIdle:     120 * time.Second,
		ReconnectDelay: 5 * time.Second,
	}
	if got := (Timing{}).withDefaults(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	custom := Timing{HTTPTimeout: time.Second, ConnectTimeout: 2 * time.Second, StreamIdle: 3 * time.Second, ReconnectDelay: 4 * time.Second}
	if got := custom.withDefaults(); got != custom {
		t.Fatalf("got %+v, want %+v", got, custom)
	}
}
