package hue

import (
	"math"
	"strings"

	"github.com/henry40408/domus/internal/core"
)

const (
	entityPrefix = "light.hue_"
	scenePrefix  = "scene.hue_"
)

// Light is one Hue light as the bridge reports it.
type Light struct {
	ID   string
	Name string
	On   bool
	// Brightness is the Hue brightness, 0.0 to 100.0.
	Brightness *float64
	// Owner is the device this light belongs to; zigbee_connectivity is
	// reported per device. Empty when unknown.
	Owner string
	// Reachable is false when the bridge cannot reach the light (unplugged,
	// out of range).
	Reachable bool
	// SupportsCT means the light has a color_temperature capability.
	SupportsCT bool
	// SupportsColor means the light has a color capability.
	SupportsColor bool
	// Mirek is the color temperature in mireds while the light is in white mode.
	Mirek *uint16
	// XY is the CIE xy color.
	XY *core.XY
}

// pctToHA converts Hue percent (0-100) to Home Assistant brightness (1-255).
func pctToHA(pct float64) int {
	return int(math.Min(math.Max(math.Round(pct/100*255), 1), 255))
}

// haToPct converts Home Assistant brightness (0-255) to Hue percent with one
// decimal.
func haToPct(brightness uint8) float64 {
	return math.Round(float64(brightness)/255*1000) / 10
}

// lightFromResource reads a light resource; it needs at least an id.
func lightFromResource(v any) (Light, bool) {
	id, ok := stringAt(v, "id")
	if !ok {
		return Light{}, false
	}
	light := Light{
		ID:            id,
		Name:          "Hue light",
		Reachable:     true,
		SupportsCT:    isObjectAt(v, "color_temperature"),
		SupportsColor: isObjectAt(v, "color"),
		Mirek:         mirekOf(v),
		XY:            xyOf(v),
	}
	if name, ok := stringAt(v, "metadata", "name"); ok {
		light.Name = name
	}
	light.On, _ = boolAt(v, "on", "on")
	if brightness, ok := floatAt(v, "dimming", "brightness"); ok {
		light.Brightness = &brightness
	}
	light.Owner, _ = stringAt(v, "owner", "rid")
	return light, true
}

// applyUpdate merges a (partial) update event into the light.
func (l *Light) applyUpdate(v any) {
	if on, ok := boolAt(v, "on", "on"); ok {
		l.On = on
	}
	if brightness, ok := floatAt(v, "dimming", "brightness"); ok {
		l.Brightness = &brightness
	}
	if name, ok := stringAt(v, "metadata", "name"); ok {
		l.Name = name
	}
	if hasKey(v, "color_temperature") {
		l.Mirek = mirekOf(v)
	} else if hasKey(v, "color") {
		// Only the color changed: the light left white mode.
		l.Mirek = nil
	}
	if xy := xyOf(v); xy != nil {
		l.XY = xy
	}
}

// mirekOf is color_temperature.mirek, unless the bridge marks it stale (the
// light is showing a color).
func mirekOf(v any) *uint16 {
	if valid, ok := boolAt(v, "color_temperature", "mirek_valid"); ok && !valid {
		return nil
	}
	mirek, ok := uint16At(v, "color_temperature", "mirek")
	if !ok {
		return nil
	}
	return &mirek
}

func xyOf(v any) *core.XY {
	x, okX := floatAt(v, "color", "xy", "x")
	y, okY := floatAt(v, "color", "xy", "y")
	if !okX || !okY {
		return nil
	}
	return &core.XY{X: x, Y: y}
}

// shortID is the first 8 characters of a Hue resource uuid, without dashes.
func shortID(uuid string) string {
	short := strings.ReplaceAll(uuid, "-", "")
	runes := []rune(short)
	if len(runes) > 8 {
		runes = runes[:8]
	}
	return string(runes)
}

// entityID is stable and derived from the Hue resource uuid, so renames never
// break groups.
func (l Light) entityID() string { return entityPrefix + shortID(l.ID) }

func (l Light) haState() string {
	switch {
	case !l.Reachable:
		return "unavailable"
	case l.On:
		return "on"
	default:
		return "off"
	}
}

// attributes are deliberately small: constrained clients (watches) must parse
// the whole payload.
func (l Light) attributes() core.Attributes {
	attributes := core.Attributes{"friendly_name": l.Name}
	if !l.Reachable {
		return attributes
	}
	modes := []string{"brightness"}
	if l.SupportsCT {
		modes = append(modes, "color_temp")
	}
	if l.SupportsColor {
		modes = append(modes, "xy")
	}
	attributes["supported_color_modes"] = modes
	if !l.On {
		return attributes
	}
	if l.Brightness != nil {
		attributes["brightness"] = pctToHA(*l.Brightness)
	}
	switch {
	case l.Mirek != nil && l.SupportsCT:
		attributes["color_mode"] = "color_temp"
		attributes["color_temp"] = int(*l.Mirek)
	case l.XY != nil && l.SupportsColor:
		attributes["color_mode"] = "xy"
		attributes["xy_color"] = []float64{l.XY.X, l.XY.Y}
	}
	return attributes
}

// requestBody is the bridge body for an action, leaving out what this light
// cannot do (a color for a white-only bulb would otherwise fail the whole
// request).
func (l Light) requestBody(action core.LightAction) map[string]any {
	return lightBody(action, l.SupportsCT, l.SupportsColor)
}

// lightBody is the CLIP v2 body for an action on a light, or on a
// grouped_light.
func lightBody(action core.LightAction, supportsCT, supportsColor bool) map[string]any {
	body := map[string]any{"on": map[string]any{"on": action.On}}
	if action.On {
		if action.Brightness != nil {
			body["dimming"] = map[string]any{"brightness": haToPct(*action.Brightness)}
		}
		if action.ColorTemp != nil && supportsCT {
			body["color_temperature"] = map[string]any{"mirek": int(*action.ColorTemp)}
		}
		if action.XY != nil && supportsColor {
			body["color"] = map[string]any{"xy": map[string]any{"x": action.XY.X, "y": action.XY.Y}}
		}
	}
	if action.TransitionMS != nil {
		body["dynamics"] = map[string]any{"duration": *action.TransitionMS}
	}
	return body
}
