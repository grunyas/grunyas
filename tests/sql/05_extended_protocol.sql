-- 05_extended_protocol.sql
-- Goal: Verify the proxy's handling of prepared statements.

-- Setup: Create a table to select from (even if proxy mocks execution, valid SQL is better)
DROP TABLE IF EXISTS test_extended;
CREATE TABLE test_extended (id int);
INSERT INTO test_extended VALUES (1);

-- Test 1: Prepare and Execute
PREPARE my_plan(int) AS SELECT * FROM test_extended WHERE id = $1;
EXECUTE my_plan(1);

-- Test 2: Deallocate
DEALLOCATE my_plan;

-- Teardown
DROP TABLE IF EXISTS test_extended;
