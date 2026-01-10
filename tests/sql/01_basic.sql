-- 01_basic.sql
-- Goal: Verify simple request-response cycles.

-- Setup: None required for basic selects

-- Test 1: Simple integer select
SELECT 1;

-- Test 2: Check server version
SELECT version();

-- Test 3: Check current user
SELECT current_user;

-- Test 4: Check current timestamp
SELECT NOW();

-- Teardown: None
