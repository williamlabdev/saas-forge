// Package internal exists for exactly one declaration: the embedded migration
// tree. It lives here, at the root of internal/, because that is the only
// directory that is an ancestor of every module's migrations/ — and go:embed
// cannot reach upward with "..".
//
// The patterns are globs, not a list of paths, and that is the whole point.
// Three separate places used to answer "which files are migrations" (the
// compose generator, its guard test, and two e2e TestMains), and a hand-typed
// list is silent when it is missing an entry — see ADR-012. Everything now
// reads this one FS, so the answer is derived in one place or nowhere.
//
// A pattern that matches nothing is a compile error, so a layout change that
// moves migrations out from under these globs cannot fail quietly either.
package internal

import "embed"

// MigrationsFS holds every .sql file under internal/*/migrations and
// internal/*/*/migrations — modules nest one or two levels deep
// (internal/user, internal/cms/content). Down files are embedded alongside up
// files: nothing applies them today, but leaving them out would make the FS
// disagree with the disk for no reason.
//
// scripts/_domain-template/migrations is deliberately outside these patterns.
// The template is a source for `make new-domain`, not a migration of this
// database.
//
//go:embed */migrations/*.sql */*/migrations/*.sql
var MigrationsFS embed.FS
