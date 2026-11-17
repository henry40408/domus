package store

import "context"

// HueBridge is the paired Hue bridge: its address and application key.
type HueBridge struct {
	IP  string
	Key string
}

// HueGet is the paired bridge (ErrNotFound when none is paired).
func (s *Store) HueGet(ctx context.Context) (HueBridge, error) {
	var bridge HueBridge
	err := s.db.QueryRowContext(ctx, "SELECT ip, app_key FROM hue WHERE id = 1").Scan(&bridge.IP, &bridge.Key)
	return bridge, notFound(err)
}

// HueSet stores the paired bridge, replacing any earlier one.
func (s *Store) HueSet(ctx context.Context, bridge HueBridge) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO hue (id, ip, app_key) VALUES (1, ?1, ?2)",
		bridge.IP, bridge.Key)
	return err
}
