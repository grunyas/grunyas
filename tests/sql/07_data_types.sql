-- 07_data_types.sql
-- Goal: Ensure data types are marshaled/unmarshaled correctly.

-- Setup: Create table with various types
DROP TABLE IF EXISTS test_types;
CREATE TABLE test_types (
    id SERIAL PRIMARY KEY,
    t_text TEXT,
    t_int INT,
    t_bool BOOLEAN,
    t_json JSONB,
    t_array INT[],
    t_date DATE,
    t_ts TIMESTAMP
);

INSERT INTO test_types (t_text, t_int, t_bool, t_json, t_array, t_date, t_ts)
VALUES ('hello', 123, true, '{"key": "value"}', ARRAY[1, 2, 3], '2023-01-01', '2023-01-01 10:00:00');

-- Test 1: Select JSONB
SELECT t_json FROM test_types;

-- Test 2: Select Array
SELECT t_array FROM test_types;

-- Test 3: Select Date/Time
SELECT t_date, t_ts FROM test_types;

-- Test 4: Select All
SELECT * FROM test_types;

-- Teardown
DROP TABLE IF EXISTS test_types;
