package db

import (
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"lineblocs.com/scheduler/internal/config"
)

// New creates a new SQLX database connection pool from the application config.
func New(cfg *config.Config) (*sqlx.DB, error) {
	db, err := sqlx.Connect("mysql", cfg.Database.DSN())
	if err != nil {
		return nil, fmt.Errorf("db: failed to connect: %w", err)
	}

	db.SetMaxOpenConns(cfg.Database.MaxOpen)
	db.SetMaxIdleConns(cfg.Database.MaxIdle)
	db.SetConnMaxLifetime(cfg.Database.Lifetime)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("db: ping failed: %w", err)
	}

	return db, nil
}
