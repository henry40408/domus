package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"unicode/utf8"

	"github.com/henry40408/domus/internal/auth"
	"github.com/henry40408/domus/internal/store"
	"github.com/henry40408/domus/internal/version"
)

// fieldMax is the longest accepted value of any text field the app reports,
// in characters.
const fieldMax = 256

// registerMobile mounts the mobile_app integration: device registration and
// the app's webhook. The webhook id is the webhook's only credential, exactly
// as in Home Assistant, and is never stored in plain.
func (a *App) registerMobile(mux *http.ServeMux) {
	mux.Handle("/api/mobile_app/registrations", a.tokenRoute(http.MethodPost, a.register))
	mux.HandleFunc("POST /api/webhook/{id}", a.webhook)
}

// scalar is a field as text; apps are not consistent about strings versus
// numbers. The empty string is a value; absent and null are nil.
func scalar(body map[string]any, key string) (*string, error) {
	var text string
	switch value := body[key].(type) {
	case nil:
		return nil, nil
	case string:
		text = value
	case json.Number:
		text = value.String()
	case bool:
		text = fmt.Sprint(value)
	default:
		return nil, invalidField(key)
	}
	if utf8.RuneCountInString(text) > fieldMax {
		return nil, invalidField(key)
	}
	return &text, nil
}

func invalidField(key string) error { return requestError("Invalid field: " + key + ".") }

// requiredText is a required, bounded field of the registration request;
// empty is allowed, absent is not.
func requiredText(body map[string]any, key string) (string, error) {
	text, err := scalar(body, key)
	if err != nil {
		return "", err
	}
	if text == nil {
		return "", requestError("Missing required field: " + key + ".")
	}
	return *text, nil
}

// optionalText is an optional field; empty counts as absent.
func optionalText(body map[string]any, key string) (*string, error) {
	text, err := scalar(body, key)
	if err != nil || text == nil || *text == "" {
		return nil, err
	}
	return text, nil
}

// registrationFields reads the device a registration request describes.
func registrationFields(body map[string]any) (store.RegistrationFields, error) {
	var f store.RegistrationFields
	var err error
	required := []struct {
		key  string
		into *string
	}{
		{"app_id", &f.AppID}, {"app_name", &f.AppName}, {"app_version", &f.AppVersion},
	}
	for _, field := range required {
		if *field.into, err = requiredText(body, field.key); err != nil {
			return f, err
		}
	}
	if f.DeviceID, err = optionalText(body, "device_id"); err != nil {
		return f, err
	}
	rest := []struct {
		key  string
		into *string
	}{
		{"device_name", &f.DeviceName}, {"manufacturer", &f.Manufacturer},
		{"model", &f.Model}, {"os_name", &f.OSName},
	}
	for _, field := range rest {
		if *field.into, err = requiredText(body, field.key); err != nil {
			return f, err
		}
	}
	f.OSVersion, err = optionalText(body, "os_version")
	return f, err
}

// parseJSONValue parses data as one JSON value, keeping numbers as written.
func parseJSONValue(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing characters")
	}
	return value, nil
}

// register is POST /api/mobile_app/registrations: it registers the calling
// app and hands out its webhook id. No secret is returned, so the app sends
// its webhooks unencrypted (the apps support that).
func (a *App) register(w http.ResponseWriter, r *http.Request, token store.TokenAuth) {
	var raw json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	// A token limited to some groups must not mint a credential that is not.
	if token.UserID == nil || token.ReadOnly || token.Scope != nil {
		writeMessage(w, http.StatusForbidden, "This token may not register devices.")
		return
	}
	value, err := parseJSONValue(raw)
	body, isObject := value.(map[string]any)
	if err != nil || !isObject {
		writeMessage(w, http.StatusBadRequest, "Expected a JSON object.")
		return
	}
	fields, err := registrationFields(body)
	if err != nil {
		writeMessage(w, http.StatusBadRequest, err.Error())
		return
	}
	webhookID := auth.RandomHex(16)
	_, err = a.core.Store().RegistrationCreate(r.Context(), store.NewRegistration{
		UserID:      *token.UserID,
		RefreshID:   token.RefreshID,
		WebhookHash: auth.SHA256Hex(webhookID),
		Fields:      fields,
	})
	if errors.Is(err, store.ErrTooManyRegistrations) {
		writeMessage(w, http.StatusTooManyRequests, "Too many registered devices; remove some first.")
		return
	}
	if err != nil {
		a.logger.Warn("registering an app device failed", "error", err)
		writeMessage(w, http.StatusInternalServerError, "Could not register.")
		return
	}
	a.audit.Info("app device registered", "user_id", *token.UserID, "device", fields.DeviceName, "app", fields.AppID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"webhook_id":    webhookID,
		"secret":        nil,
		"cloudhook_url": nil,
		"remote_ui_url": nil,
	})
}

// zeroFloat is written as 0.0: Go would print a float64 zero as 0, and the
// apps tell a Float from an Integer.
var zeroFloat = json.RawMessage("0.0")

// haConfig is what get_config tells the app. domus has no location of its
// own, so those are zeros.
func haConfig() map[string]any {
	return map[string]any{
		"latitude":  zeroFloat,
		"longitude": zeroFloat,
		"elevation": 0,
		"unit_system": map[string]string{
			"length":                    "km",
			"mass":                      "g",
			"pressure":                  "Pa",
			"temperature":               "°C",
			"volume":                    "L",
			"wind_speed":                "m/s",
			"accumulated_precipitation": "mm",
		},
		"location_name": "domus",
		"time_zone":     "UTC",
		"components":    []string{"api", "mobile_app", "light", "scene"},
		"version":       version.HAVersion,
	}
}

func registrationJSON(f store.RegistrationFields) map[string]any {
	return map[string]any{
		"app_id":              f.AppID,
		"app_name":            f.AppName,
		"app_version":         f.AppVersion,
		"device_id":           f.DeviceID,
		"device_name":         f.DeviceName,
		"manufacturer":        f.Manufacturer,
		"model":               f.Model,
		"os_name":             f.OSName,
		"os_version":          f.OSVersion,
		"supports_encryption": false,
	}
}

// webhook is POST /api/webhook/{id}: {"type": ..., "data": ...} from the app.
// An unknown id gets 410, which tells the app its registration is gone; the
// id has 128 bits, so this is no oracle. The id is checked before the body is
// read.
func (a *App) webhook(w http.ResponseWriter, r *http.Request) {
	st := a.core.Store()
	reg, err := st.RegistrationByWebhook(r.Context(), auth.SHA256Hex(r.PathValue("id")))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			a.logger.Warn("webhook lookup failed", "error", err)
		}
		w.WriteHeader(http.StatusGone)
		return
	}
	data, ok := readBody(w, r)
	if !ok {
		return
	}
	value, err := parseJSONValue(data)
	body, isObject := value.(map[string]any)
	if err != nil || !isObject {
		writeMessage(w, http.StatusBadRequest, "Expected a JSON object.")
		return
	}
	kind, isString := body["type"].(string)
	if !isString {
		writeMessage(w, http.StatusBadRequest, "Missing type.")
		return
	}
	a.answerWebhook(w, r, reg, kind, body["data"])
}

func (a *App) answerWebhook(w http.ResponseWriter, r *http.Request, reg store.Registration, kind string, data any) {
	switch kind {
	case "get_config":
		writeJSON(w, http.StatusOK, haConfig())
	case "update_registration":
		a.updateRegistration(w, r, reg, data)
	case "register_sensor":
		writeJSON(w, http.StatusCreated, map[string]bool{"success": true})
	case "update_sensor_states":
		writeJSON(w, http.StatusOK, sensorResults(data))
	case "update_location":
		w.WriteHeader(http.StatusNoContent)
	case "get_zones":
		writeJSON(w, http.StatusOK, []any{})
	default:
		// Accepted and dropped: nothing in domus consumes them yet, and a
		// newer app must not break.
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

func (a *App) updateRegistration(w http.ResponseWriter, r *http.Request, reg store.Registration, data any) {
	update, isObject := data.(map[string]any)
	if !isObject || len(update) == 0 {
		writeMessage(w, http.StatusBadRequest, "Nothing to update.")
		return
	}
	fields := reg.RegistrationFields
	for key, slot := range map[string]*string{
		"app_version":  &fields.AppVersion,
		"device_name":  &fields.DeviceName,
		"manufacturer": &fields.Manufacturer,
		"model":        &fields.Model,
	} {
		if text, err := optionalText(update, key); err == nil && text != nil {
			*slot = *text
		}
	}
	if text, err := optionalText(update, "os_version"); err == nil && text != nil {
		fields.OSVersion = text
	}
	if err := a.core.Store().RegistrationUpdate(r.Context(), reg.ID, fields); err != nil {
		a.logger.Warn("updating an app device failed", "error", err)
	}
	writeJSON(w, http.StatusOK, registrationJSON(fields))
}

// sensorResults acknowledges every sensor that has a unique_id.
func sensorResults(data any) map[string]any {
	results := map[string]any{}
	sensors, _ := data.([]any)
	for _, sensor := range sensors {
		fields, isObject := sensor.(map[string]any)
		if !isObject {
			continue
		}
		if id, isString := fields["unique_id"].(string); isString {
			results[id] = map[string]bool{"success": true}
		}
	}
	return results
}
