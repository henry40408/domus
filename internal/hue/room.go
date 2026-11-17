package hue

import "slices"

// Room is a Hue room or zone, whose lights can be driven together through its
// grouped_light.
type Room struct {
	Name string
	// Kind is "room" or "zone".
	Kind string
	// GroupedLight is the uuid of the room's grouped_light service.
	GroupedLight string
	// Children are the resources the bridge lists: rooms list their devices,
	// zones their lights. They are resolved to light uuids on sync.
	Children []resourceRef
}

type resourceRef struct{ Type, ID string }

// roomFromResource reads a room or zone resource; it needs a name and a
// grouped_light service.
func roomFromResource(v any, kind string) (Room, bool) {
	groupedLight := ""
	for _, service := range refsAt(v, "services") {
		if service.Type == "grouped_light" {
			groupedLight = service.ID
			break
		}
	}
	name, hasName := stringAt(v, "metadata", "name")
	if groupedLight == "" || !hasName {
		return Room{}, false
	}
	return Room{Name: name, Kind: kind, GroupedLight: groupedLight, Children: refsAt(v, "children")}, true
}

// refsAt reads a list of {rtype, rid} references, skipping malformed entries.
func refsAt(v any, key string) []resourceRef {
	var refs []resourceRef
	for _, item := range arrayAt(v, key) {
		kind, okKind := stringAt(item, "rtype")
		id, okID := stringAt(item, "rid")
		if okKind && okID {
			refs = append(refs, resourceRef{Type: kind, ID: id})
		}
	}
	return refs
}

// lightIDs are the sorted uuids of the lights in this room. owners maps a
// device uuid to its lights.
func (r Room) lightIDs(owners map[string][]string) []string {
	var ids []string
	for _, child := range r.Children {
		switch child.Type {
		case "light":
			ids = append(ids, child.ID)
		case "device":
			ids = append(ids, owners[child.ID]...)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
