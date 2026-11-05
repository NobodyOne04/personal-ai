// Package postgres implements the repository ports on PostgreSQL using pgx.
// Persistence records are mapped to and from domain entities inside this
// package and never leak out of it. Prompts and generated text are never
// stored.
package postgres
