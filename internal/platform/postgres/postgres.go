package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"jungle-gaming/internal/config"
)

var Module = fx.Module("postgres", fx.Provide(New))

type Database struct{ pool *pgxpool.Pool }

func New(lc fx.Lifecycle, c config.Config) (*Database, error) {
	poolConfig, err := pgxpool.ParseConfig(c.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL connection configuration")
	}
	poolConfig.ConnConfig.ConnectTimeout = c.DependencyTimeout
	poolConfig.MaxConns = 10
	db := &Database{}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, c.DependencyTimeout)
			defer cancel()
			pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
			if err != nil {
				return fmt.Errorf("create PostgreSQL pool failed")
			}
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				return fmt.Errorf("PostgreSQL startup check failed")
			}
			db.pool = pool
			return nil
		},
		OnStop: func(context.Context) error { db.pool.Close(); return nil },
	})
	return db, nil
}

func (d *Database) Check(ctx context.Context) error { return d.pool.Ping(ctx) }

// Pool is available after the Fx startup hook. Its lifecycle remains owned by Database.
func (d *Database) Pool() *pgxpool.Pool { return d.pool }
