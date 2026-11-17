package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type loginBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func jsonRequest(contentType, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

func TestDecodeJSONRejectionsMatchAxum(t *testing.T) {
	for _, c := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"valid", "application/json", `{"username":"x","password":"y"}`, 0},
		{"charset parameter", "application/json; charset=utf-8", `{}`, 0},
		{"json suffix", "application/vnd.api+json", `{}`, 0},
		{"unknown fields are ignored", "application/json", `{"extra":1}`, 0},
		{"no content type", "", `{}`, http.StatusUnsupportedMediaType},
		{"form content type", "application/x-www-form-urlencoded", `{}`, http.StatusUnsupportedMediaType},
		{"text content type", "text/plain", `{}`, http.StatusUnsupportedMediaType},
		{"syntax error", "application/json", `{nope`, http.StatusBadRequest},
		{"empty body", "application/json", ``, http.StatusBadRequest},
		{"trailing data", "application/json", `{} {}`, http.StatusBadRequest},
		{"wrong type", "application/json", `{"username":5}`, http.StatusUnprocessableEntity},
		{"body over 2 MB", "application/json", `{"username":"` + strings.Repeat("a", 2<<20) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var dst loginBody
			ok := decodeJSON(rec, jsonRequest(c.contentType, c.body), &dst)
			if ok != (c.status == 0) {
				t.Fatalf("ok = %v, want %v", ok, c.status == 0)
			}
			if c.status != 0 && rec.Code != c.status {
				t.Errorf("status %d, want %d (%s)", rec.Code, c.status, rec.Body)
			}
		})
	}
}

func TestDecodeJSONRejectionTextsAreTheRustOnes(t *testing.T) {
	rec := httptest.NewRecorder()
	decodeJSON(rec, jsonRequest("text/plain", `{}`), &loginBody{})
	if got, want := bodyString(rec), "Expected request with `Content-Type: application/json`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content type %q", got)
	}
}

func TestDecodeJSONFillsTheTarget(t *testing.T) {
	var dst loginBody
	if !decodeJSON(httptest.NewRecorder(), jsonRequest("application/json", `{"username":"u","password":"p"}`), &dst) {
		t.Fatal("rejected")
	}
	if dst != (loginBody{Username: "u", Password: "p"}) {
		t.Errorf("got %+v", dst)
	}
}

func TestDecodeFormRejectionsMatchAxum(t *testing.T) {
	const form = "application/x-www-form-urlencoded"
	for _, c := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"valid", form, "grant_type=refresh_token&refresh_token=a%20b", 0},
		{"charset parameter", form + "; charset=UTF-8", "a=1", 0},
		{"json content type", "application/json", `{}`, http.StatusUnsupportedMediaType},
		{"no content type", "", "a=1", http.StatusUnsupportedMediaType},
		{"bad escape", form, "a=%zz", http.StatusUnprocessableEntity},
		{"body over 2 MB", form, "a=" + strings.Repeat("b", 2<<20), http.StatusRequestEntityTooLarge},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			values, ok := decodeForm(rec, jsonRequest(c.contentType, c.body))
			if ok != (c.status == 0) {
				t.Fatalf("ok = %v, want %v", ok, c.status == 0)
			}
			if c.status != 0 && rec.Code != c.status {
				t.Errorf("status %d, want %d", rec.Code, c.status)
			}
			if c.name == "valid" && values.Get("refresh_token") != "a b" {
				t.Errorf("values %v", values)
			}
		})
	}
}

func TestWriteJSONIsPlainSerdeStyle(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]string{"k": "<a&b>"})
	if rec.Code != http.StatusCreated || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := rec.Body.String(); got != `{"k":"<a&b>"}` {
		t.Errorf("body %q", got)
	}
}

func TestWriteMessageUsesTheAdminErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMessage(rec, http.StatusForbidden, "Admin only.")
	if rec.Code != http.StatusForbidden || rec.Body.String() != `{"message":"Admin only."}` {
		t.Errorf("%d %q", rec.Code, rec.Body)
	}
}
