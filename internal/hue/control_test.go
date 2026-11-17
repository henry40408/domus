package hue

import (
	"log/slog"
	"testing"

	"github.com/henry40408/domus/internal/core"
)

func TestALightThatSupportsColorGetsColorInItsRequest(t *testing.T) {
	mock := newMock()
	mock.set("light", richLightJSON("aaaaaaaa-0000", "dev-a"))
	env := newEnv(t, mock, Timing{})
	env.sync(t)

	action := core.LightAction{On: true, ColorTemp: ptr(uint16(250)), XY: &core.XY{X: 0.3, Y: 0.2}}
	noError(t, env.core.CallLight(ctx, "light.hue_aaaaaaaa", action))
	puts := mock.recordedPuts()
	if len(puts) != 1 {
		t.Fatalf("got %+v", puts)
	}
	sameJSON(t, puts[0].Body, J{
		"on":                J{"on": true},
		"color_temperature": J{"mirek": 250.0},
		"color":             J{"xy": J{"x": 0.3, "y": 0.2}},
	})
}

func TestANilLoggerMeansTheDefaultAndAGivenOneIsKept(t *testing.T) {
	given := quietLogger()
	c := newCore(t)
	tests := map[string]struct{ nilLogger, givenLogger *slog.Logger }{
		"client": {
			NewClient("http://bridge", "key", Timing{}, nil).logger,
			NewClient("http://bridge", "key", Timing{}, given).logger,
		},
		"integration": {
			NewIntegration(IntegrationConfig{Core: c}).logger,
			NewIntegration(IntegrationConfig{Core: c, Logger: given}).logger,
		},
		"manager": {NewManager(c, Timing{}, nil).logger, NewManager(c, Timing{}, given).logger},
	}
	for name, tt := range tests {
		if tt.nilLogger != slog.Default() {
			t.Errorf("%s: a nil logger must become slog.Default()", name)
		}
		if tt.givenLogger != given {
			t.Errorf("%s: a given logger must be kept", name)
		}
	}
}
