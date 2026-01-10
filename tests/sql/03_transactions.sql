-- 03_transactions.sql
-- Goal: Verify transaction state consistency across the proxy.

-- Setup: Clean up any previous run
DROP TABLE IF EXISTS test_txn;

-- Test 1: Basic Commit
BEGIN;
SELECT 1;
COMMIT;

-- Test 2: Rollback with Data
BEGIN;
CREATE TABLE test_txn (id int);
INSERT INTO test_txn VALUES (1);
-- Verify data is visible inside transaction
SELECT count(*) FROM test_txn;
ROLLBACK;

-- Test 3: Verify Rollback (Table should not exist)
-- This might return an error if the table doesn't exist, which is expected behavior for a successful rollback of CREATE TABLE
-- To make it "checkable" without failing the script hard in some runners, we can check pg_tables, but direct select failing is also a valid test result.
SELECT count(*) FROM pg_tables WHERE tablename = 'test_txn';

-- Teardown
DROP TABLE IF EXISTS test_txn;
