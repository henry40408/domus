package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// maxBodyBytes is axum's default limit for the Json and Form extractors.
const maxBodyBytes = 2 << 20

// writeJSON writes v as JSON the way serde_json does: no HTML escaping and no
// trailing newline.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "cannot encode the response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// writeMessage writes the admin API's error body, {"message": ...}.
func writeMessage(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

// decodeJSON reads a JSON request body into dst and, like axum's Json
// extractor, answers a plain-text rejection itself when it cannot: 415 for
// the wrong Content-Type, 413 for a body over 2 MB, 400 for a syntax error
// (and an empty body), 422 for a value of the wrong type. It reports whether
// the caller may go on. A missing field is not an error here; handlers that
// require one answer 422 themselves.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !hasMediaType(r, isJSONMediaType) {
		http.Error(w, "Expected request with `Content-Type: application/json`", http.StatusUnsupportedMediaType)
		return false
	}
	body, ok := readBody(w, r)
	if !ok {
		return false
	}
	return unmarshalStrict(w, body, dst)
}

func unmarshalStrict(w http.ResponseWriter, body []byte, dst any) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	err := dec.Decode(dst)
	if err == nil && dec.More() {
		err = errors.New("trailing characters")
	}
	if err == nil {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		http.Error(w, "Failed to deserialize the JSON body into the target type: "+err.Error(), http.StatusUnprocessableEntity)
		return false
	}
	http.Error(w, "Failed to parse the request body as JSON: "+err.Error(), http.StatusBadRequest)
	return false
}

// decodeForm reads an application/x-www-form-urlencoded body like axum's Form
// extractor: 415 for the wrong Content-Type, 413 for a body over 2 MB, 422
// for an invalid encoding.
func decodeForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if !hasMediaType(r, func(t string) bool { return t == "application/x-www-form-urlencoded" }) {
		http.Error(w, "Form requests must have `Content-Type: application/x-www-form-urlencoded`", http.StatusUnsupportedMediaType)
		return nil, false
	}
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, "Failed to deserialize form body: "+err.Error(), http.StatusUnprocessableEntity)
		return nil, false
	}
	return values, true
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err == nil {
		return body, true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		http.Error(w, "length limit exceeded", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	http.Error(w, "Failed to buffer the request body", http.StatusBadRequest)
	return nil, false
}

func hasMediaType(r *http.Request, accept func(mediaType string) bool) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && accept(mediaType)
}

// isJSONMediaType matches application/json and application/*+json.
func isJSONMediaType(mediaType string) bool {
	subtype, ok := strings.CutPrefix(mediaType, "application/")
	return ok && (subtype == "json" || strings.HasSuffix(subtype, "+json"))
}
