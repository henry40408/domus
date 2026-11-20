package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestCreatingASessionKeepsTheOnesThatAreStillActive(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	noError(t, s.CreateSession(ctx, "first", id, ""))
	noError(t, s.CreateSession(ctx, "second", id, ""))
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM sessions"); rows != 2 {
		t.Fatalf("sessions = %d, the sweep must only take expired and idle ones", rows)
	}
}

func newDeviceRegistration(userID int64, n int, deviceID *string) NewRegistration {
	return NewRegistration{
		UserID:      userID,
		WebhookHash: fmt.Sprintf("hook-%d", n),
		Fields:      RegistrationFields{AppID: "app", AppName: "App", DeviceName: "phone", DeviceID: deviceID},
	}
}

func TestTheRegistrationLimitIsExactlyTwenty(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	for n := 0; n < maxRegistrationsPerUser; n++ {
		if _, err := s.RegistrationCreate(ctx, newDeviceRegistration(id, n, nil)); err != nil {
			t.Fatalf("registration %d of %d refused: %v", n+1, maxRegistrationsPerUser, err)
		}
	}
	_, err = s.RegistrationCreate(ctx, newDeviceRegistration(id, 99, nil))
	if !errors.Is(err, ErrTooManyRegistrations) {
		t.Fatalf("the next one: %v, want ErrTooManyRegistrations", err)
	}
}

func TestRegistrationsWithoutADeviceIdNeverReplaceEachOther(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	_, err = s.RegistrationCreate(ctx, newDeviceRegistration(id, 1, nil))
	noError(t, err)
	_, err = s.RegistrationCreate(ctx, newDeviceRegistration(id, 2, nil))
	noError(t, err)
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM registrations"); rows != 2 {
		t.Fatalf("registrations = %d, want 2", rows)
	}
}

func TestARegistrationWithTheSameDeviceIdReplacesTheOldOne(t *testing.T) {
	s := openMemory(t)
	id, err := s.CreateUser(ctx, "u", "h", false)
	noError(t, err)
	device := "device-1"
	first, err := s.RegistrationCreate(ctx, newDeviceRegistration(id, 1, &device))
	noError(t, err)
	second, err := s.RegistrationCreate(ctx, newDeviceRegistration(id, 2, &device))
	noError(t, err)
	if first != second {
		t.Fatalf("ids %d and %d, the registration is replaced in place", first, second)
	}
	if rows := queryInt(t, s, "SELECT COUNT(*) FROM registrations"); rows != 1 {
		t.Fatalf("registrations = %d, want 1", rows)
	}
}
