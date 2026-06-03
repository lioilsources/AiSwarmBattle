package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lioilsources/shaderbattle/pkg/types"
)

const ttl = 7 * 24 * time.Hour

// ShaderCache persists prompt → shader response mappings in SQLite.
type ShaderCache struct {
	db *sql.DB
}

func New(db *sql.DB) (*ShaderCache, error) {
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("cache: init schema: %w", err)
	}
	return &ShaderCache{db: db}, nil
}

func (c *ShaderCache) Get(ctx context.Context, req *types.ShaderRequest) (*types.ShaderResponse, bool, error) {
	key := cacheKey(req)
	row := c.db.QueryRowContext(ctx,
		`SELECT response_json FROM shader_cache WHERE cache_key = ? AND created_at > ? AND seed = ?`,
		key, time.Now().Add(-ttl).Unix(), req.Seed,
	)

	var raw string
	if err := row.Scan(&raw); err == sql.ErrNoRows {
		return nil, false, nil
	} else if err != nil {
		return nil, false, fmt.Errorf("cache: get: %w", err)
	}

	_, _ = c.db.ExecContext(ctx,
		`UPDATE shader_cache SET hit_count = hit_count + 1 WHERE cache_key = ? AND seed = ?`,
		key, req.Seed,
	)

	var resp types.ShaderResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, false, fmt.Errorf("cache: unmarshal: %w", err)
	}
	return &resp, true, nil
}

func (c *ShaderCache) Set(ctx context.Context, req *types.ShaderRequest, resp *types.ShaderResponse) error {
	key := cacheKey(req)
	raw, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("cache: marshal: %w", err)
	}
	_, err = c.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO shader_cache (cache_key, seed, request_json, response_json, shader_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key, req.Seed, mustJSON(req), string(raw), resp.ShaderID, time.Now().Unix(),
	)
	return err
}

func cacheKey(req *types.ShaderRequest) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d|%v|%v|%s",
		req.Prompt,
		req.Constraints.MaxMobileCost,
		req.Seed,
		req.Constraints.RequireCamera,
		req.Constraints.RequireAnimated,
		req.Constraints.AspectRatio,
	)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const schema = `
CREATE TABLE IF NOT EXISTS shader_cache (
	cache_key     TEXT NOT NULL,
	seed          INTEGER NOT NULL,
	request_json  TEXT NOT NULL,
	response_json TEXT NOT NULL,
	shader_id     TEXT,
	created_at    INTEGER NOT NULL,
	hit_count     INTEGER DEFAULT 0,
	PRIMARY KEY (cache_key, seed)
);
CREATE INDEX IF NOT EXISTS idx_cache_created ON shader_cache(created_at);
`
