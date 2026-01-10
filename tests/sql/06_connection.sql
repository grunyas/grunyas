-- 06_connection.sql
-- Goal: Test session reset and termination.

-- Setup: None

-- Test 1: Discard All
DISCARD ALL;

-- Test 2: Reset All
RESET ALL;

-- Test 3: Set Application Name
SET application_name = 'test_client';
SHOW application_name;

-- Teardown: None
