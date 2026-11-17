package store

import (
	"reflect"
	"testing"

	"github.com/henry40408/domus/internal/auth"
)

func openRustCreated(t *testing.T) *Store {
	t.Helper()
	s, err := Open(ctx, copyFixture(t, "rust-created.db"))
	noError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRustHashedPasswordsVerifyThroughUserLogin(t *testing.T) {
	s := openRustCreated(t)
	logins := []struct {
		username, password string
		admin              bool
		scope              Scope
	}{
		{"admin", "Fixture-Admin-Pass-1", true, nil},
		{"alice", "愛麗絲的密碼是很長很長的字串", false, scopeOf("bedroom-lights")},
		{"bob", "bob$pass word!2026 ✓", false, nil},
		{"dave", "dave-changed-pass-2", false, scopeOf()},
	}
	for _, login := range logins {
		t.Run(login.username, func(t *testing.T) {
			user, hash, err := s.UserLogin(ctx, login.username)
			noError(t, err)
			if !auth.VerifyPassword(login.password, hash) {
				t.Fatal("the Rust hash does not verify")
			}
			if user.IsAdmin != login.admin || !reflect.DeepEqual(user.Scope, login.scope) {
				t.Fatalf("user = %+v", user)
			}
		})
	}
}

func TestRustTokensAuthenticateByTheirPlaintext(t *testing.T) {
	s := openRustCreated(t)
	tokens := []struct {
		plaintext string
		readOnly  bool
		scope     Scope
	}{
		{"domus_dcf20250ccb0b5562c86574163e674cd", false, nil},
		{"domus_befc1cc614f8d2e95fda5242bf62d99a", false, scopeOf("living")},
		{"domus_349c77d4975b115bff3f189c7a009b0a", true, nil},
		{"domus_e28c3d567d1f384aba67c678d1826f37", false, scopeOf("bedroom-lights")},
	}
	for _, token := range tokens {
		got, err := s.TokenAuth(ctx, auth.SHA256Hex(token.plaintext))
		noError(t, err)
		if got.ReadOnly != token.readOnly || !reflect.DeepEqual(got.Scope, token.scope) {
			t.Errorf("%s = %+v", token.plaintext, got)
		}
	}
	// token 5 expired a second after it was made
	if _, err := s.TokenAuth(ctx, auth.SHA256Hex("domus_4c2cecd67ded518651ebe60c565dd5f5")); !isNotFound(err) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestRustRegistrationsAreFoundByTheirWebhookId(t *testing.T) {
	s := openRustCreated(t)
	first := must(s.RegistrationByWebhook(ctx, auth.SHA256Hex("b56c67a890f195d3518c0318421822fb")))
	if first.ID != 1 || first.UserID != 1 || first.DeviceName != "Pixel" {
		t.Fatalf("registration = %+v", first)
	}
	tablet := must(s.RegistrationByWebhook(ctx, auth.SHA256Hex("cc6650989ed7ded0140fb6e89eb1e5c9")))
	if tablet.ID != 3 || tablet.DeviceName != "Tablet" {
		t.Fatalf("registration = %+v", tablet)
	}
	if _, err := s.RegistrationByWebhook(ctx, "unknown"); !isNotFound(err) {
		t.Fatalf("unknown webhook: %v", err)
	}
}

func TestRustGroupKeepsItsMemberOrder(t *testing.T) {
	s := openRustCreated(t)
	want := []string{"light.hue_a1000003", "light.hue_a1000001", "scene.hue_b2000002", "light.hue_a1000002"}
	if got := must(s.GroupMembers(ctx, "living")); !reflect.DeepEqual(got, want) {
		t.Fatalf("members = %v", got)
	}
	if !must(s.GroupExposed(ctx, "living")) {
		t.Fatal("living is exposed as a light")
	}
}
