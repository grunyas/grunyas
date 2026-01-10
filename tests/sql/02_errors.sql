-- 02_errors.sql
-- Goal: Ensure errors from the backend are correctly propagated to the client.

-- Setup: Ensure table does not exist (SAFE to drop if exists just in case)
DROP TABLE IF EXISTS non_existent_table;

-- Test 1: Select from non-existent table (Should Error)
SELECT * FROM non_existent_table;

-- Test 2: Division by zero (Should Error)
SELECT 1/0;

-- Test 3: Syntax error (Should Error)
SELEC 1;

-- Teardown: None
