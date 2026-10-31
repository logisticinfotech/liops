// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"net/url"
	"strconv"
)

// DBs lists the database servers the monitor found, and whether it can read them.
func (c *Client) DBs(ctx context.Context) ([]DBStatus, error) {
	var out []DBStatus
	return out, c.get(ctx, "/v1/db", &out)
}

// Queries returns the query shapes with the most time over a range, for one server and
// database ("" for all).
func (c *Client) Queries(ctx context.Context, rng, server, db string, limit int) ([]QueryTotal, error) {
	q := url.Values{"range": {rng}, "server": {server}, "db": {db}, "limit": {strconv.Itoa(limit)}}
	var out []QueryTotal
	return out, c.get(ctx, "/v1/db/queries?"+q.Encode(), &out)
}

// Query returns one query shape and its 5-minute history over a range.
func (c *Client) Query(ctx context.Context, rng, server, db, id string) (QueryDetailResponse, error) {
	q := url.Values{"range": {rng}, "server": {server}, "db": {db}, "id": {id}}
	var out QueryDetailResponse
	return out, c.get(ctx, "/v1/db/query?"+q.Encode(), &out)
}

// Sample reads a MySQL query shape's sample statement, live. It holds real values:
// show it, never store or log it.
func (c *Client) Sample(ctx context.Context, server, db, id string) (QuerySample, error) {
	q := url.Values{"server": {server}, "db": {db}, "id": {id}}
	var out QuerySample
	return out, c.get(ctx, "/v1/db/sample?"+q.Encode(), &out)
}

// Activity reads what a server is running now, live.
func (c *Client) Activity(ctx context.Context, server string) (Activity, error) {
	var out Activity
	return out, c.get(ctx, "/v1/db/activity?"+url.Values{"server": {server}}.Encode(), &out)
}

// Tables returns a server's latest table and index snapshot.
func (c *Client) Tables(ctx context.Context, server string) (DBTables, error) {
	var out DBTables
	return out, c.get(ctx, "/v1/db/tables?"+url.Values{"server": {server}}.Encode(), &out)
}
