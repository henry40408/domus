package store

import (
	"reflect"
	"testing"

	"github.com/henry40408/domus/internal/auth"
)

type login struct {
	username, password string
	admin              bool
	scope              Scope
}

func addLogin(t *testing.T, s *Store, l login) {
	t.Helper()
	id, err := s.CreateUser(ctx, l.username, auth.HashPassword(l.password), l.admin)
	noError(t, err)
	if l.scope != nil {
		noError(t, s.SetUserScope(ctx, id, l.scope))
	}
}

// The caller verifies the password against the hash UserLogin returns, so the
// hash must come back byte for byte, whatever characters the password has.
func TestUserLoginReturnsAHashThatVerifiesTheirPassword(t *testing.T) {
	s := openMemory(t)
	logins := []login{
		{"admin", "admin-pass-1", true, nil},
		{"alice", "愛麗絲的密碼是很長很長的字串", false, scopeOf("bedroom-lights")},
		{"bob", "bob$pass word!2026 ✓", false, nil},
		{"dave", "dave-changed-pass-2", false, scopeOf()},
	}
	for _, l := range logins {
		addLogin(t, s, l)
	}
	for _, l := range logins {
		t.Run(l.username, func(t *testing.T) {
			user, hash, err := s.UserLogin(ctx, l.username)
			noError(t, err)
			if !auth.VerifyPassword(l.password, hash) {
				t.Fatal("the stored hash does not verify the password")
			}
			if user.IsAdmin != l.admin || !reflect.DeepEqual(user.Scope, l.scope) {
				t.Fatalf("user = %+v", user)
			}
		})
	}
}

func TestTokensAuthenticateByTheHashOfTheirPlaintext(t *testing.T) {
	s := openMemory(t)
	admin, err := s.CreateUser(ctx, "admin", "h", true)
	noError(t, err)
	alice, err := s.CreateUser(ctx, "alice", "h", false)
	noError(t, err)
	noError(t, s.SetUserScope(ctx, alice, scopeOf("bedroom-lights")))
	tokens := []struct {
		plaintext string
		owner     int64
		readOnly  bool
		scope     Scope
	}{
		{"domus_dcf20250ccb0b5562c86574163e674cd", admin, false, nil},
		{"domus_befc1cc614f8d2e95fda5242bf62d99a", admin, false, scopeOf("living")},
		{"domus_349c77d4975b115bff3f189c7a009b0a", admin, true, nil},
		{"domus_e28c3d567d1f384aba67c678d1826f37", alice, false, scopeOf("bedroom-lights")},
	}
	for _, token := range tokens {
		_, err := s.CreateToken(ctx, NewToken{
			UserID: token.owner, Name: "t", TokenHash: auth.SHA256Hex(token.plaintext),
			Scope: token.scope, ReadOnly: token.readOnly,
		})
		noError(t, err)
	}
	// Last, because creating a token sweeps the expired ones away.
	const expiredPlaintext = "domus_4c2cecd67ded518651ebe60c565dd5f5"
	_, err = s.CreateToken(ctx, NewToken{
		UserID: admin, Name: "expired", TokenHash: auth.SHA256Hex(expiredPlaintext), Expires: ptr(nowSecs() - 1),
	})
	noError(t, err)

	for _, token := range tokens {
		got, err := s.TokenAuth(ctx, auth.SHA256Hex(token.plaintext))
		noError(t, err)
		if got.ReadOnly != token.readOnly || !reflect.DeepEqual(got.Scope, token.scope) {
			t.Errorf("%s = %+v", token.plaintext, got)
		}
	}
	if _, err := s.TokenAuth(ctx, auth.SHA256Hex(expiredPlaintext)); !isNotFound(err) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestRegistrationsAreFoundByTheHashOfTheirWebhookId(t *testing.T) {
	s := openMemory(t)
	admin, err := s.CreateUser(ctx, "admin", "h", true)
	noError(t, err)
	bob, err := s.CreateUser(ctx, "bob", "h", false)
	noError(t, err)
	devices := []struct {
		owner         int64
		webhook, name string
	}{
		{admin, "b56c67a890f195d3518c0318421822fb", "Pixel"},
		{bob, "2fa168a086cd1799c6c2bdcc88c05bae", "Bob phone"},
		{admin, "cc6650989ed7ded0140fb6e89eb1e5c9", "Tablet"},
	}
	ids := make([]int64, len(devices))
	for i, device := range devices {
		ids[i], err = s.RegistrationCreate(ctx, NewRegistration{
			UserID: device.owner, WebhookHash: auth.SHA256Hex(device.webhook),
			Fields: RegistrationFields{AppID: "app", AppName: "App", DeviceName: device.name},
		})
		noError(t, err)
	}

	for i, device := range devices {
		got, err := s.RegistrationByWebhook(ctx, auth.SHA256Hex(device.webhook))
		noError(t, err)
		if got.ID != ids[i] || got.UserID != device.owner || got.DeviceName != device.name {
			t.Errorf("%s = %+v", device.name, got)
		}
	}
	if _, err := s.RegistrationByWebhook(ctx, "unknown"); !isNotFound(err) {
		t.Fatalf("unknown webhook: %v", err)
	}
}

func TestGroupMembersComeBackInPositionOrder(t *testing.T) {
	s := openMemory(t)
	exec(t, s, "INSERT INTO groups (name, expose_light) VALUES ('living', 1)")
	// Inserted in neither position nor name order, so only pos can sort them.
	exec(t, s, `INSERT INTO group_members (group_name, entity_id, pos) VALUES
	    ('living', 'light.hue_a1000002', 3), ('living', 'scene.hue_b2000002', 2),
	    ('living', 'light.hue_a1000003', 0), ('living', 'light.hue_a1000001', 1)`)

	want := []string{"light.hue_a1000003", "light.hue_a1000001", "scene.hue_b2000002", "light.hue_a1000002"}
	got, err := s.GroupMembers(ctx, "living")
	noError(t, err)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("members = %v", got)
	}
	exposed, err := s.GroupExposed(ctx, "living")
	noError(t, err)
	if !exposed {
		t.Fatal("living is exposed as a light")
	}
}
