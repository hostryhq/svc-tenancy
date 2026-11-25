-- Cluster provisioning of the tenancy database roles (ADR-0003 section 6, ADR-0013 V3). Run once as a
-- superuser per cluster, before `tenancy migrate`; deployment owns it (AM12: not run anywhere yet).
--
--   psql -v migrator_password=... -v app_password=... -v vendor_password=... -v relay_password=... -f provision-roles.sql
--
-- Passwords come from the secret store by reference; never put a value in this file.
--
--   tenancy_migrator  owns the database and its tables; runs migrations only (never at serve time)
--   tenancy_app       DML only, NOSUPERUSER NOBYPASSRLS, not an owner (the service's runtime role)
--   tenancy_vendor    SELECT on the two directory tables and nothing else
--   tenancy_relay     the outbox table only (relay to the bus)

SELECT format('CREATE ROLE tenancy_migrator LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'migrator_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tenancy_migrator') \gexec
SELECT format('CREATE ROLE tenancy_app LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'app_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tenancy_app') \gexec
SELECT format('CREATE ROLE tenancy_vendor LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'vendor_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tenancy_vendor') \gexec
SELECT format('CREATE ROLE tenancy_relay LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB PASSWORD %L', :'relay_password')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tenancy_relay') \gexec

SELECT 'CREATE DATABASE tenancy OWNER tenancy_migrator'
 WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'tenancy') \gexec
