# Atlas project configuration (Community Edition, Apache-2.0 workflow).
#
# The desired schema lives in db/schema.sql (shared with sqlc). Versioned
# migrations are generated into migrations/ via `atlas migrate diff` and
# applied with `atlas migrate apply`. A throwaway "dev" Postgres is required
# at authoring time for diffing; set ATLAS_DEV_URL or pass --dev-url.
#
# See apps/api/README.md for the exact commands.

data "external_schema" "sql" {
  program = [
    "sh", "-c",
    "cat db/schema.sql",
  ]
}

env "local" {
  src = "file://db/schema.sql"
  dev = getenv("ATLAS_DEV_URL")
  url = getenv("DATABASE_URL")
  migration {
    dir = "file://migrations"
  }
  format {
    migrate {
      diff = "{{ sql . \"  \" }}"
    }
  }
}
