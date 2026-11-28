package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ALi3naTEd0/couchlog/internal/model"
)

// Subscription is what the user pays for one service.
type Subscription struct {
	Service    string
	PriceCents int64
	Currency   string // ISO 4217
	Period     string // month | year
	UpdatedAt  time.Time
}

var reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// Subscriptions returns the user's saved prices by service.
func (s *Store) Subscriptions(ctx context.Context, userID int64) (map[string]Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT service, price_cents, currency, period, updated_at
		FROM subscriptions WHERE user_id = ?`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	out := map[string]Subscription{}
	for rows.Next() {
		var x Subscription
		var updated int64
		if err := rows.Scan(&x.Service, &x.PriceCents, &x.Currency, &x.Period, &updated); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		x.UpdatedAt = fromMS(updated)
		out[x.Service] = x
	}
	return out, rows.Err()
}

// SetSubscription saves a price; a nil sub (or price 0) forgets it.
func (s *Store) SetSubscription(ctx context.Context, userID int64, service string, sub *Subscription, now time.Time) error {
	if service == "" || len(service) > 80 {
		return errors.New("store: service name")
	}
	if sub == nil || sub.PriceCents == 0 {
		return s.exec(ctx, "DELETE FROM subscriptions WHERE user_id = ? AND service = ?", userID, service)
	}
	if sub.PriceCents < 0 || !reCurrency.MatchString(sub.Currency) || (sub.Period != "month" && sub.Period != "year") {
		return fmt.Errorf("store: subscription %+v", *sub)
	}
	return s.exec(ctx, `INSERT INTO subscriptions(user_id, service, price_cents, currency, period, updated_at)
		VALUES (?,?,?,?,?,?) ON CONFLICT (user_id, service) DO UPDATE SET price_cents = excluded.price_cents,
		currency = excluded.currency, period = excluded.period, updated_at = excluded.updated_at`,
		userID, service, sub.PriceCents, sub.Currency, sub.Period, ms(now))
}

// Setting returns a user setting, or def when unset.
func (s *Store) Setting(ctx context.Context, userID int64, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE user_id = ? AND key = ?", userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return def, fmt.Errorf("store: %w", err)
	}
	return v, nil
}

// SetSetting saves a user setting.
func (s *Store) SetSetting(ctx context.Context, userID int64, key, value string) error {
	return s.exec(ctx, `INSERT INTO settings(user_id, key, value) VALUES (?,?,?)
		ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, userID, key, value)
}

// SetHidden hides a session from the history ("ignored") or shows it again
// (""). Hiding does not touch what was already sent.
func (s *Store) SetHidden(ctx context.Context, id int64, h model.Hidden) error {
	if h != model.Shown && h != model.Ignored {
		return fmt.Errorf("store: hidden %q", h)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE sessions SET hidden = ? WHERE id = ?", string(h), id)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSession
	}
	return nil
}

// SettingsWithPrefix returns the user's settings whose key starts with
// prefix, by the rest of the key.
func (s *Store) SettingsWithPrefix(ctx context.Context, userID int64, prefix string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE user_id = ? AND substr(key, 1, ?) = ?",
		userID, len(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		out[k[len(prefix):]] = v
	}
	return out, rows.Err()
}

// DeleteSetting forgets a user setting.
func (s *Store) DeleteSetting(ctx context.Context, userID int64, key string) error {
	return s.exec(ctx, "DELETE FROM settings WHERE user_id = ? AND key = ?", userID, key)
}
