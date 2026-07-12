-- Runs once, on an empty data directory. The bootstrap superuser is `postgres`;
-- the application role is not, or row-level security would be bypassed and every
-- tenant-isolation test would pass against a database enforcing nothing.
CREATE ROLE muallim LOGIN PASSWORD 'muallim' NOSUPERUSER NOBYPASSRLS;
CREATE DATABASE muallim OWNER muallim;
CREATE DATABASE muallim_test OWNER muallim;
