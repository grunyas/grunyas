-- Grunyas Simulator shared schema
-- Each simulator mounts this file into PostgreSQL's docker-entrypoint-initdb.d/

CREATE TABLE IF NOT EXISTS users (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE NOT NULL,
    balance NUMERIC(12,2) DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
    id SERIAL PRIMARY KEY,
    user_id INT REFERENCES users(id),
    amount NUMERIC(12,2) NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS events (
    id SERIAL PRIMARY KEY,
    type TEXT NOT NULL,
    payload JSONB,
    created_at TIMESTAMPTZ DEFAULT now()
);

-- Seed data: 1000 users
INSERT INTO users (name, email, balance)
SELECT 'user_' || i, 'user_' || i || '@test.com', (random() * 10000)::numeric(12,2)
FROM generate_series(1, 1000) AS i
ON CONFLICT DO NOTHING;
