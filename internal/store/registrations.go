package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrTooManyRegistrations means the user already has the most devices
// registered.
var ErrTooManyRegistrations = errors.New("too many registrations")

// RegistrationFields is what a Companion app tells us about itself and its
// device when registering.
type RegistrationFields struct {
	AppID        string  `json:"app_id"`
	AppName      string  `json:"app_name"`
	AppVersion   string  `json:"app_version"`
	DeviceID     *string `json:"device_id"`
	DeviceName   string  `json:"device_name"`
	Manufacturer string  `json:"manufacturer"`
	Model        string  `json:"model"`
	OSName       string  `json:"os_name"`
	OSVersion    *string `json:"os_version"`
}

// Registration is a registered device. The fields are flattened in JSON.
type Registration struct {
	// ID is the public id (the row id); the webhook hash never leaves the
	// store.
	ID       int64   `json:"id"`
	UserID   int64   `json:"user_id"`
	Username *string `json:"username"`
	RegistrationFields
	Created int64 `json:"created"`
	// LastSeen is the Unix seconds of the last webhook call (coarse:
	// refreshed at most once a minute).
	LastSeen *int64 `json:"last_seen"`
}

// NewRegistration is a device to register. WebhookHash is the hash of the
// webhook id; RefreshID is the app login it came with, if any.
type NewRegistration struct {
	UserID      int64
	RefreshID   *int64
	WebhookHash string
	Fields      RegistrationFields
}

// registrationSelect is the SELECT of a registration (columns in the order
// scanRegistration reads) plus tail.
func registrationSelect(tail string) string {
	return `SELECT r.id, r.user_id, u.username, r.app_id, r.app_name, r.app_version,
                r.device_id, r.device_name, r.manufacturer, r.model, r.os_name, r.os_version,
                r.created, r.last_seen
             FROM registrations r LEFT JOIN users u ON u.id = r.user_id ` + tail
}

func scanRegistration(row rowScanner) (Registration, error) {
	var r Registration
	err := row.Scan(&r.ID, &r.UserID, &r.Username, &r.AppID, &r.AppName, &r.AppVersion,
		&r.DeviceID, &r.DeviceName, &r.Manufacturer, &r.Model, &r.OSName, &r.OSVersion,
		&r.Created, &r.LastSeen)
	return r, err
}

// RegistrationCreate registers a device and returns its id. An app that
// reports a device_id it registered with before replaces that registration
// (its old webhook id stops working) instead of adding another one.
// ErrTooManyRegistrations when the user has no room left.
func (s *Store) RegistrationCreate(ctx context.Context, reg NewRegistration) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := createRegistration(ctx, tx, reg, s.now())
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func createRegistration(ctx context.Context, tx *sql.Tx, reg NewRegistration, now int64) (int64, error) {
	f := reg.Fields
	if f.DeviceID != nil {
		existing, err := existingRegistration(ctx, tx, reg)
		if err == nil {
			return existing, replaceRegistration(ctx, tx, existing, reg)
		}
		if !errors.Is(err, ErrNotFound) {
			return 0, err
		}
	}
	var count int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM registrations WHERE user_id = ?1", reg.UserID).
		Scan(&count); err != nil {
		return 0, err
	}
	if count >= maxRegistrationsPerUser {
		return 0, ErrTooManyRegistrations
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO registrations (webhook_hash, user_id, refresh_id, app_id, app_name,
                 app_version, device_id, device_name, manufacturer, model, os_name, os_version, created)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13)`,
		reg.WebhookHash, reg.UserID, reg.RefreshID, f.AppID, f.AppName,
		f.AppVersion, f.DeviceID, f.DeviceName, f.Manufacturer, f.Model, f.OSName, f.OSVersion, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func existingRegistration(ctx context.Context, tx *sql.Tx, reg NewRegistration) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM registrations WHERE user_id = ?1 AND app_id = ?2 AND device_id = ?3",
		reg.UserID, reg.Fields.AppID, reg.Fields.DeviceID).Scan(&id)
	return id, notFound(err)
}

func replaceRegistration(ctx context.Context, tx *sql.Tx, id int64, reg NewRegistration) error {
	f := reg.Fields
	_, err := tx.ExecContext(ctx,
		`UPDATE registrations SET webhook_hash = ?2, refresh_id = ?3, app_name = ?4,
                 app_version = ?5, device_name = ?6, manufacturer = ?7, model = ?8,
                 os_name = ?9, os_version = ?10, last_seen = NULL WHERE id = ?1`,
		id, reg.WebhookHash, reg.RefreshID, f.AppName, f.AppVersion, f.DeviceName,
		f.Manufacturer, f.Model, f.OSName, f.OSVersion)
	return err
}

// RegistrationByWebhook is the registration behind a webhook id hash, marking
// it seen (at most once a minute).
func (s *Store) RegistrationByWebhook(ctx context.Context, webhookHash string) (Registration, error) {
	reg, err := scanRegistration(s.db.QueryRowContext(ctx,
		registrationSelect("WHERE r.webhook_hash = ?1"), webhookHash))
	if err != nil {
		return Registration{}, notFound(err)
	}
	now := s.now()
	// Best effort: a missed update only leaves last_seen stale.
	_, _ = s.db.ExecContext(ctx,
		`UPDATE registrations SET last_seen = ?2
             WHERE id = ?1 AND (last_seen IS NULL OR last_seen < ?3)`,
		reg.ID, now, now-useTouchSecs)
	return reg, nil
}

// RegistrationUpdate replaces the descriptive fields the app reports after
// registering (name, versions, ...).
func (s *Store) RegistrationUpdate(ctx context.Context, id int64, f RegistrationFields) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE registrations SET app_version = ?2, device_name = ?3, manufacturer = ?4,
                 model = ?5, os_version = ?6 WHERE id = ?1`,
		id, f.AppVersion, f.DeviceName, f.Manufacturer, f.Model, f.OSVersion)
	return err
}

// ListRegistrations lists all registrations, or only those of userID when
// given.
func (s *Store) ListRegistrations(ctx context.Context, userID *int64) ([]Registration, error) {
	rows, err := s.db.QueryContext(ctx,
		registrationSelect("WHERE ?1 IS NULL OR r.user_id = ?1 ORDER BY r.id"), userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var registrations []Registration
	for rows.Next() {
		reg, err := scanRegistration(rows)
		if err != nil {
			return nil, err
		}
		registrations = append(registrations, reg)
	}
	return registrations, rows.Err()
}

// DeleteRegistration removes a registration so its webhook stops working;
// with userID only if it is theirs (ErrNotFound otherwise). When the
// registration came with an app login, that login ends too: otherwise the app
// would just register again with its still-valid token.
func (s *Store) DeleteRegistration(ctx context.Context, id int64, userID *int64) error {
	var refreshID sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT refresh_id FROM registrations WHERE id = ?1 AND (?2 IS NULL OR user_id = ?2)",
		id, userID).Scan(&refreshID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if refreshID.Valid {
		return s.refreshDelete(ctx, refreshID.Int64)
	}
	return affected(s.db.ExecContext(ctx,
		"DELETE FROM registrations WHERE id = ?1 AND (?2 IS NULL OR user_id = ?2)", id, userID))
}
