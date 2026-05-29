// Package postgres contains the outbound adapter that implements
// persistence-related ports against PostgreSQL.
//
// This file embeds the SQL migration files into the binary so that a
// deployed opengate binary can migrate a fresh database without any
// external files on disk. The embedded filesystem is consumed by the
// migrate subcommand in cmd/opengate.
package postgres

import "embed"

// Migrations holds the goose SQL migration files embedded at build time.
// The directory structure is preserved by go:embed, so callers pass
// "migrations" as the directory argument to goose.
//
//go:embed migrations/*.sql
var Migrations embed.FS
