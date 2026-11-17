package hue

import "github.com/henry40408/domus/internal/core"

const smartSceneType = "smart_scene"

// Scene is a Hue scene or smart scene.
type Scene struct {
	ID   string
	Name string
	// Group is the name of the room or zone the scene belongs to, if known.
	Group  *string
	Active bool
	// Smart marks a Hue smart scene (such as Natural light): its own resource
	// type and recall action.
	Smart bool
}

// sceneFromResource reads a scene resource. groups maps room/zone uuid to its
// name.
func sceneFromResource(v any, groups map[string]string) (Scene, bool) {
	id, ok := stringAt(v, "id")
	if !ok {
		return Scene{}, false
	}
	scene := Scene{ID: id, Name: "Hue scene"}
	if name, ok := stringAt(v, "metadata", "name"); ok {
		scene.Name = name
	}
	if rid, ok := stringAt(v, "group", "rid"); ok {
		if name, known := groups[rid]; known {
			scene.Group = &name
		}
	}
	if active, found := sceneActive(v); found {
		scene.Active = active
	}
	scene.Smart = resourceType(v) == smartSceneType
	return scene, true
}

func resourceType(v any) string {
	kind, _ := stringAt(v, "type")
	return kind
}

// sceneActive reads a scene's status.active (inactive, static or
// dynamic_palette) or a smart scene's state (inactive or active).
func sceneActive(v any) (active, found bool) {
	status, ok := stringAt(v, "status", "active")
	if !ok {
		status, ok = stringAt(v, "state")
	}
	return status != "inactive", ok
}

func (s Scene) entityID() string { return scenePrefix + shortID(s.ID) }

func (s Scene) friendlyName() string {
	if s.Group == nil {
		return s.Name
	}
	return *s.Group + ": " + s.Name
}

func (s Scene) attributes() core.Attributes {
	return core.Attributes{"friendly_name": s.friendlyName()}
}

// link is a zigbee_connectivity resource: which device it reports for and
// whether the device can be reached.
type link struct {
	Device    string
	Reachable bool
}

// connectivityOf reads a zigbee_connectivity resource. Only a disconnected or
// troubled link counts as unreachable; unidirectional_incoming still carries
// commands.
func connectivityOf(v any) (id string, l link, ok bool) {
	id, okID := stringAt(v, "id")
	device, okDevice := stringAt(v, "owner", "rid")
	status, okStatus := stringAt(v, "status")
	if !okID || !okDevice || !okStatus {
		return "", link{}, false
	}
	return id, link{Device: device, Reachable: reachableStatus(status)}, true
}

func reachableStatus(status string) bool {
	return status != "disconnected" && status != "connectivity_issue"
}
