package core

import (
	"context"
	"sync"

	"github.com/henry40408/domus/internal/timefmt"
)

// CallLight runs a light action. A group light fans out to its real members in
// parallel and succeeds if at least one member did; real lights go through
// their integration. ErrNoIntegration means nobody owns the entity.
func (c *Core) CallLight(ctx context.Context, entityID string, action LightAction) error {
	name, ok := c.exposedGroupOf(ctx, entityID)
	if !ok {
		return c.callRealLight(ctx, entityID, action)
	}
	err := c.CallLights(ctx, c.realMembers(ctx, name), action)
	if err != nil {
		c.logger.Warn("group light call failed", "group", name, "error", err)
	}
	return err
}

// CallLights runs an action on several real lights: one request when they form
// a light set the integration can address together, otherwise one call per
// light in parallel. It succeeds if at least one light did; when all failed it
// returns the error of the first member.
func (c *Core) CallLights(ctx context.Context, members []string, action LightAction) error {
	handled, err := c.callLightSet(ctx, members, action)
	if handled {
		if err != nil {
			return err
		}
		for _, member := range members {
			c.applyOptimistic(member, action)
		}
		return nil
	}
	errs := c.callEach(ctx, members, action)
	var firstErr error
	anyOK := len(members) == 0
	for i, err := range errs {
		if err == nil {
			anyOK = true
			continue
		}
		c.logger.Warn("light call failed", "entity_id", members[i], "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	if anyOK {
		return nil
	}
	return firstErr
}

// callEach calls every member at once and returns the errors in member order.
func (c *Core) callEach(ctx context.Context, members []string, action LightAction) []error {
	errs := make([]error, len(members))
	var wg sync.WaitGroup
	for i, member := range members {
		wg.Go(func() {
			errs[i] = c.callRealLight(ctx, member, action)
		})
	}
	wg.Wait()
	return errs
}

// callLightSet uses the integration that owns every one of entityIDs, if there
// is one and it can address them together.
func (c *Core) callLightSet(ctx context.Context, entityIDs []string, action LightAction) (bool, error) {
	if len(entityIDs) == 0 {
		return false, nil
	}
	integration, err := c.ownerOf(entityIDs[0])
	if err != nil {
		return false, nil
	}
	for _, entityID := range entityIDs {
		if !integration.Owns(entityID) {
			return false, nil
		}
	}
	return integration.CallLightSet(ctx, entityIDs, action)
}

func (c *Core) ownerOf(entityID string) (Integration, error) {
	for _, integration := range c.registeredIntegrations() {
		if integration.Owns(entityID) {
			return integration, nil
		}
	}
	return nil, ErrNoIntegration
}

// ActivateScene recalls a scene; its state becomes the activation time (the
// Home Assistant convention).
func (c *Core) ActivateScene(ctx context.Context, entityID string) error {
	integration, err := c.ownerOf(entityID)
	if err != nil {
		return err
	}
	if err := integration.ActivateScene(ctx, entityID); err != nil {
		return err
	}
	c.MarkSceneActivated(entityID)
	return nil
}

// MarkSceneActivated sets a scene's state to now, keeping its attributes.
func (c *Core) MarkSceneActivated(entityID string) {
	old, ok := c.GetState(entityID)
	if !ok {
		return
	}
	c.SetState(entityID, timefmt.RFC3339Micros(c.now()), old.Attributes)
}

// callRealLight runs a light action through the owning integration, then
// applies it optimistically.
func (c *Core) callRealLight(ctx context.Context, entityID string, action LightAction) error {
	integration, err := c.ownerOf(entityID)
	if err != nil {
		return err
	}
	if err := integration.CallLight(ctx, entityID, action); err != nil {
		return err
	}
	c.applyOptimistic(entityID, action)
	return nil
}

// applyOptimistic reflects an accepted action in the state right away; the
// event stream confirms it later.
func (c *Core) applyOptimistic(entityID string, action LightAction) {
	old, ok := c.GetState(entityID)
	if !ok {
		return
	}
	// An unreachable light stays unavailable until the bridge says otherwise.
	if old.State == "unavailable" {
		return
	}
	attributes := old.Attributes
	if !action.On {
		for _, key := range []string{"brightness", "color_mode", "color_temp", "xy_color"} {
			delete(attributes, key)
		}
		c.SetState(entityID, "off", attributes)
		return
	}
	if action.Brightness != nil {
		attributes["brightness"] = int(*action.Brightness)
	}
	if action.ColorTemp != nil {
		attributes["color_mode"] = "color_temp"
		attributes["color_temp"] = int(*action.ColorTemp)
		delete(attributes, "xy_color")
	}
	if action.XY != nil {
		attributes["color_mode"] = "xy"
		attributes["xy_color"] = []float64{action.XY.X, action.XY.Y}
		delete(attributes, "color_temp")
	}
	c.SetState(entityID, "on", attributes)
}
