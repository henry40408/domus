package server

import (
	"math"
	"strings"

	"github.com/henry40408/domus/internal/core"
)

const (
	// Hue's supported color temperature range, in mireds (6535 K to 2000 K).
	minMirek = 153.0
	maxMirek = 500.0
	// maxTransitionMS is the longest fade accepted, so a typo cannot park a
	// light for hours.
	maxTransitionMS = 3_600_000.0
)

// requestError is a message for the caller: the REST API shows it as is, so
// it reads like a sentence.
type requestError string

func (e requestError) Error() string { return string(e) }

// field reads a key of a decoded request body. The body may be any JSON
// value; only an object has fields.
func field(body any, key string) (any, bool) {
	object, ok := body.(map[string]any)
	if !ok {
		return nil, false
	}
	value, present := object[key]
	return value, present
}

// number is Value::as_f64: only JSON numbers count.
func number(v any) (float64, bool) {
	n, ok := v.(float64)
	return n, ok
}

// requestedEntities collects entity ids from entity_id (string or array) and
// target.entity_id.
func requestedEntities(body any) ([]string, error) {
	out := []string{}
	target, _ := field(body, "target")
	for _, source := range []any{body, target} {
		value, present := field(source, "entity_id")
		if !present {
			continue
		}
		ids, err := entityIDsOf(value)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return removeEmpty(out), nil
}

func entityIDsOf(value any) ([]string, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case string:
		parts := strings.Split(v, ",")
		for i, part := range parts {
			parts[i] = strings.TrimSpace(part)
		}
		return parts, nil
	case []any:
		ids := make([]string, 0, len(v))
		for _, item := range v {
			id, ok := item.(string)
			if !ok {
				return nil, requestError("entity_id must be a string")
			}
			ids = append(ids, id)
		}
		return ids, nil
	default:
		return nil, requestError("entity_id must be a string or list of strings")
	}
}

func removeEmpty(ids []string) []string {
	out := ids[:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func clampedByte(v float64) uint8 {
	return uint8(math.Min(math.Max(math.Round(v), 0), 255))
}

func requestedBrightness(body any) *uint8 {
	if b, ok := field(body, "brightness"); ok {
		if n, isNumber := number(b); isNumber {
			v := clampedByte(n)
			return &v
		}
	}
	if p, ok := field(body, "brightness_pct"); ok {
		if n, isNumber := number(p); isNumber {
			v := clampedByte(n / 100 * 255)
			return &v
		}
	}
	return nil
}

// requestedTransition is the fade time in ms from transition (seconds, like
// Home Assistant).
func requestedTransition(body any) (*uint32, error) {
	value, present := field(body, "transition")
	if !present || value == nil {
		return nil, nil
	}
	seconds, ok := number(value)
	if !ok || seconds < 0 {
		return nil, requestError("transition must be a non-negative number of seconds.")
	}
	ms := uint32(math.Round(math.Min(seconds*1000, maxTransitionMS)))
	return &ms, nil
}

func numberPair(v any) (a, b float64, ok bool) {
	pair, isArray := v.([]any)
	if !isArray || len(pair) != 2 {
		return 0, 0, false
	}
	a, okA := number(pair[0])
	b, okB := number(pair[1])
	return a, b, okA && okB
}

func mirek(m float64) *uint16 {
	v := uint16(math.Round(math.Min(math.Max(m, minMirek), maxMirek)))
	return &v
}

func inUnitRange(v float64) bool { return v >= 0 && v <= 1 }

// requestedColor is the color a turn_on asks for, as mireds or xy. Home
// Assistant accepts several spellings; the first one present wins.
func requestedColor(body any) (colorTemp *uint16, xy *core.XY, err error) {
	if v, ok := field(body, "color_temp"); ok {
		m, isNumber := number(v)
		if !isNumber || m <= 0 {
			return nil, nil, requestError("color_temp must be mireds.")
		}
		return mirek(m), nil, nil
	}
	for _, key := range []string{"color_temp_kelvin", "kelvin"} {
		v, ok := field(body, key)
		if !ok {
			continue
		}
		k, isNumber := number(v)
		if !isNumber || k <= 0 {
			return nil, nil, requestError("Kelvin must be a positive number.")
		}
		return mirek(1_000_000 / k), nil, nil
	}
	if v, ok := field(body, "xy_color"); ok {
		x, y, isPair := numberPair(v)
		if !isPair || !inUnitRange(x) || !inUnitRange(y) {
			return nil, nil, requestError("xy_color must be [x, y] between 0 and 1.")
		}
		return nil, &core.XY{X: x, Y: y}, nil
	}
	if v, ok := field(body, "hs_color"); ok {
		h, s, isPair := numberPair(v)
		if !isPair {
			return nil, nil, requestError("hs_color must be [hue, saturation].")
		}
		x, y := rgbToXY(hsToRGB(h, s))
		return nil, &core.XY{X: x, Y: y}, nil
	}
	if v, ok := field(body, "rgb_color"); ok {
		channels, valid := rgbChannels(v)
		if !valid {
			return nil, nil, requestError("rgb_color must be [r, g, b] between 0 and 255.")
		}
		x, y := rgbToXY(channels[0], channels[1], channels[2])
		return nil, &core.XY{X: x, Y: y}, nil
	}
	return nil, nil, nil
}

func rgbChannels(v any) ([3]float64, bool) {
	var channels [3]float64
	items, isArray := v.([]any)
	if !isArray || len(items) != 3 {
		return channels, false
	}
	for i, item := range items {
		n, isNumber := number(item)
		if !isNumber || n < 0 || n > 255 {
			return channels, false
		}
		channels[i] = n
	}
	return channels, true
}

// lightAction is the action of a turn_on or turn_off request. A nil action
// with no error means the service does not exist.
func lightAction(service string, body any) (*core.LightAction, error) {
	transition, err := requestedTransition(body)
	if err != nil {
		return nil, err
	}
	switch service {
	case "turn_on":
		colorTemp, xy, err := requestedColor(body)
		if err != nil {
			return nil, err
		}
		return &core.LightAction{
			On:           true,
			Brightness:   requestedBrightness(body),
			ColorTemp:    colorTemp,
			XY:           xy,
			TransitionMS: transition,
		}, nil
	case "turn_off":
		return &core.LightAction{TransitionMS: transition}, nil
	default:
		return nil, nil
	}
}
