package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func ListGroups(ctx context.Context, pool *pgxpool.Pool) ([]Group, error) {
	rows, err := pool.Query(ctx, `select id::text, name from groups order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func GroupExists(ctx context.Context, pool *pgxpool.Pool, id string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx, `select exists(select 1 from groups where id = $1::uuid)`, id).Scan(&exists)
	return exists, err
}
