-- Version 2: what you pay for each service, and your own settings.

CREATE TABLE subscriptions (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  service     TEXT    NOT NULL,                  -- netflix | hbomax | youtube | ... (as the dashboard groups apps)
  price_cents INTEGER NOT NULL CHECK (price_cents >= 0),
  currency    TEXT    NOT NULL,                  -- ISO 4217: MXN, USD, EUR
  period      TEXT    NOT NULL DEFAULT 'month' CHECK (period IN ('month', 'year')),
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (user_id, service)
);

CREATE TABLE settings (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT    NOT NULL,
  value   TEXT    NOT NULL,
  PRIMARY KEY (user_id, key)
);
