package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Client struct {
	pool *pgxpool.Pool
}

// ConnectTimeout bounds the whole connect — pool create plus the first Ping,
// which does DNS resolution, the TCP dial, and the postgres startup handshake.
// Without a deadline a hung resolver, a black-holed connection (NetworkPolicy),
// or a driver panic makes the operator die SILENTLY at startup — exit 2, no log.
const ConnectTimeout = 10 * time.Second

// NewClient dials postgres, bounded by ConnectTimeout so a hang or a driver
// panic surfaces as a clear error rather than a silent crash. The connect runs
// in a goroutine (so a blocked Ping that ignores ctx cannot wedge the caller)
// with a recover (so a driver panic becomes an error, not a process abort).
func NewClient(ctx context.Context, uri string) (*Client, error) {
	connectCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	type result struct {
		c   *Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- result{err: fmt.Errorf("postgres: connect panicked: %v", r)}
			}
		}()
		pool, err := pgxpool.New(connectCtx, uri)
		if err != nil {
			ch <- result{err: fmt.Errorf("postgres: connect: %w", err)}
			return
		}
		if err := pool.Ping(connectCtx); err != nil {
			pool.Close()
			ch <- result{err: fmt.Errorf("postgres: ping: %w", err)}
			return
		}
		ch <- result{c: &Client{pool: pool}}
	}()

	select {
	case r := <-ch:
		return r.c, r.err
	case <-connectCtx.Done():
		// Do not echo uri — it carries the password.
		return nil, fmt.Errorf("postgres: connect timed out after %s (DNS, network egress, or a NetworkPolicy blocking the connection?): %w", ConnectTimeout, connectCtx.Err())
	}
}

func (c *Client) Migrate(ctx context.Context) error {
	if _, err := c.pool.Exec(ctx, migrateSQL); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}
	return nil
}

func (c *Client) Close() {
	c.pool.Close()
}

func (c *Client) Pool() *pgxpool.Pool { return c.pool }
