-- 04_large_results.sql
-- Goal: Test handling of multiple rows and large payloads.

-- Setup: None

-- Test 1: Generate 1000 rows
SELECT generate_series(1, 1000);

-- Test 2: Large text payload
SELECT repeat('A', 1000) FROM generate_series(1, 10);

-- Teardown: None
