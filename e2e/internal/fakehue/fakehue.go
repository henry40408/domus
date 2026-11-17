// Package fakehue is a tiny stand-in for a Hue Bridge (CLIP v2), just enough
// for domus to pair with it and list a believable house. Writes are accepted
// and ignored.
package fakehue

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
)

type light struct {
	id, name, owner, kind string
	on                    bool
	brightness            float64
	mirek                 int
	x, y                  float64
}

var lights = []light{
	{"a1000001-0000-4000-8000-000000000001", "Ceiling", "dev-1", "color", true, 80, 366, 0.46, 0.41},
	{"a1000002-0000-4000-8000-000000000002", "Floor lamp", "dev-2", "color", true, 55, 454, 0.52, 0.41},
	{"a1000003-0000-4000-8000-000000000003", "TV strip", "dev-3", "color", true, 40, 300, 0.17, 0.20},
	{"a1000004-0000-4000-8000-000000000004", "Bedside left", "dev-4", "ct", true, 30, 400, 0, 0},
	{"a1000005-0000-4000-8000-000000000005", "Bedside right", "dev-5", "ct", false, 100, 370, 0, 0},
	{"a1000006-0000-4000-8000-000000000006", "Desk lamp", "dev-6", "ct", true, 100, 250, 0, 0},
	{"a1000007-0000-4000-8000-000000000007", "Monitor light", "dev-7", "color", false, 70, 300, 0.31, 0.33},
}

type room struct {
	id, groupedLight, name string
	devices                []string
}

var rooms = []room{
	{"c3000001-0000-4000-8000-000000000001", "d4000001-0000-4000-8000-000000000001", "Living Room", []string{"dev-1", "dev-2", "dev-3"}},
	{"c3000002-0000-4000-8000-000000000002", "d4000002-0000-4000-8000-000000000002", "Bedroom", []string{"dev-4", "dev-5"}},
	{"c3000003-0000-4000-8000-000000000003", "d4000003-0000-4000-8000-000000000003", "Office", []string{"dev-6", "dev-7"}},
}

type scene struct{ id, name, roomID string }

var scenes = []scene{
	{"b2000001-0000-4000-8000-000000000001", "Relax", rooms[0].id},
	{"b2000002-0000-4000-8000-000000000002", "Movie night", rooms[0].id},
	{"b2000003-0000-4000-8000-000000000003", "Energize", rooms[0].id},
	{"b2000004-0000-4000-8000-000000000004", "Nightlight", rooms[1].id},
	{"b2000005-0000-4000-8000-000000000005", "Read", rooms[1].id},
	{"b2000006-0000-4000-8000-000000000006", "Focus", rooms[2].id},
	{"b2000007-0000-4000-8000-000000000007", "Concentrate", rooms[2].id},
}

type object = map[string]any

func lightJSON(l light) object {
	v := object{
		"id": l.id, "type": "light", "metadata": object{"name": l.name}, "on": object{"on": l.on},
		"dimming": object{"brightness": l.brightness},
		"owner":   object{"rid": l.owner, "rtype": "device"},
	}
	v["color_temperature"] = object{"mirek": l.mirek, "mirek_valid": true}
	if l.kind == "color" {
		v["color"] = object{"xy": object{"x": l.x, "y": l.y}}
	}
	return v
}

func roomJSON(r room) object {
	children := []object{}
	for _, device := range r.devices {
		children = append(children, object{"rid": device, "rtype": "device"})
	}
	return object{
		"id": r.id, "type": "room", "metadata": object{"name": r.name},
		"children": children,
		"services": []object{{"rid": r.groupedLight, "rtype": "grouped_light"}},
	}
}

func sceneJSON(s scene) object {
	return object{
		"id": s.id, "type": "scene", "metadata": object{"name": s.name},
		"group": object{"rid": s.roomID, "rtype": "room"}, "status": object{"active": "inactive"},
	}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func data[T any](items []T, convert func(T) object) object {
	list := []object{}
	for _, item := range items {
		list = append(list, convert(item))
	}
	return object{"errors": []any{}, "data": list}
}

func emptyData() object { return object{"errors": []any{}, "data": []any{}} }

// Start serves the fake bridge on an ephemeral loopback port; Close stops it.
// Its URL is what domus is paired with.
func Start() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /clip/v2/resource/light", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, data(lights, lightJSON))
	})
	mux.HandleFunc("GET /clip/v2/resource/room", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, data(rooms, roomJSON))
	})
	mux.HandleFunc("GET /clip/v2/resource/scene", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, data(scenes, sceneJSON))
	})
	for _, kind := range []string{"zone", "smart_scene", "zigbee_connectivity"} {
		mux.HandleFunc("GET /clip/v2/resource/"+kind, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, emptyData())
		})
	}
	for _, kind := range []string{"light", "scene", "grouped_light"} {
		mux.HandleFunc("PUT /clip/v2/resource/"+kind+"/{id}", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, emptyData())
		})
	}
	// Held open and silent, like a bridge with nothing to report.
	mux.HandleFunc("GET /eventstream/clip/v2", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /api", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []object{{"success": object{"username": "demo-app-key", "clientkey": "x"}}})
	})
	return httptest.NewServer(mux)
}
