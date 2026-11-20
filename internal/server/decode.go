package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
		writeText(w, http.StatusInternalServerError, "cannot encode the response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// writeOK writes the admin API's success body, {"ok":true}.
func writeOK(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
	_, ok := decodeJSONBody(w, r, dst)
	return ok
}

// decodeJSONBody is decodeJSON that also returns the raw body, for handlers
// that report a missing field with rejectMissing.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) ([]byte, bool) {
	if !hasMediaType(r, isJSONMediaType) {
		writeText(w, http.StatusUnsupportedMediaType, "Expected request with `Content-Type: application/json`")
		return nil, false
	}
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	return body, unmarshalStrict(w, body, dst)
}

// requiredField is a field of a request body and whether the body had it.
type requiredField struct {
	name    string
	present bool
}

// rejectMissing answers 422 for the first absent field, worded like axum's
// Json extractor (serde reports the position where the object ends). It
// reports whether it rejected the request.
func rejectMissing(w http.ResponseWriter, body []byte, fields ...requiredField) bool {
	for _, field := range fields {
		if field.present {
			continue
		}
		end := bytes.LastIndexByte(body, '}') + 1
		line := 1 + bytes.Count(body[:end], []byte("\n"))
		column := end - (bytes.LastIndexByte(body[:end], '\n') + 1)
		writeText(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("Failed to deserialize the JSON body into the target type: missing field `%s` at line %d column %d",
				field.name, line, column))
		return true
	}
	return false
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
		writeText(w, http.StatusUnprocessableEntity, "Failed to deserialize the JSON body into the target type: "+err.Error())
		return false
	}
	writeText(w, http.StatusBadRequest, "Failed to parse the request body as JSON: "+err.Error())
	return false
}

// decodeForm reads an application/x-www-form-urlencoded body like axum's Form
// extractor: 415 for the wrong Content-Type, 413 for a body over 2 MB, 422
// for an invalid encoding.
func decodeForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if !hasMediaType(r, func(t string) bool { return t == "application/x-www-form-urlencoded" }) {
		writeText(w, http.StatusUnsupportedMediaType, "Form requests must have `Content-Type: application/x-www-form-urlencoded`")
		return nil, false
	}
	body, ok := readBody(w, r)
	if !ok {
		return nil, false
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		writeText(w, http.StatusUnprocessableEntity, "Failed to deserialize form body: "+err.Error())
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
		writeText(w, http.StatusRequestEntityTooLarge, "length limit exceeded")
		return nil, false
	}
	writeText(w, http.StatusBadRequest, "Failed to buffer the request body")
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
