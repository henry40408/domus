package core

import (
	"context"
	"errors"
)

// ErrNoIntegration means no registered integration owns the entity.
var ErrNoIntegration = errors.New("no integration owns this entity")

// XY is a CIE xy chromaticity.
type XY struct{ X, Y float64 }

// LightAction is a turn_on or turn_off request. Unset fields leave the light as
// it is. A turn-off only uses TransitionMS.
type LightAction struct {
	// On is true for turn_on.
	On bool
	// Brightness is the Home Assistant brightness, 0-255.
	Brightness *uint8
	// ColorTemp is the color temperature in mireds (Hue's mirek).
	ColorTemp *uint16
	XY        *XY
	// TransitionMS is the fade time in milliseconds.
	TransitionMS *uint32
}

// LightSet is a group of lights an integration can address in one request,
// such as a Hue room or zone.
type LightSet struct {
	Name string `json:"name"`
	// Kind is "room" or "zone".
	Kind      string   `json:"kind"`
	EntityIDs []string `json:"entity_ids"`
}

// Integration is a source of entities that can also run service calls for them.
// Methods may do network I/O; the core never holds a lock while calling them.
type Integration interface {
	Owns(entityID string) bool
	CallLight(ctx context.Context, entityID string, action LightAction) error
	// LightSets are the sets the integration can control with a single request.
	LightSets() []LightSet
	// CallLightSet runs one action on several lights in a single request when
	// the integration can (such as a Hue room). handled=false means it cannot
	// and the caller goes light by light.
	CallLightSet(ctx context.Context, entityIDs []string, action LightAction) (handled bool, err error)
	// ActivateScene recalls a scene.
	ActivateScene(ctx context.Context, entityID string) error
}

// BaseIntegration is the default for the optional Integration methods: no
// light sets, no batch calls, no scenes. Embed it and implement Owns and
// CallLight.
type BaseIntegration struct{}

// LightSets reports none.
func (BaseIntegration) LightSets() []LightSet { return nil }

// CallLightSet is never handled.
func (BaseIntegration) CallLightSet(context.Context, []string, LightAction) (bool, error) {
	return false, nil
}

// ActivateScene fails: scenes are not supported.
func (BaseIntegration) ActivateScene(context.Context, string) error {
	return errors.New("scenes are not supported")
}
