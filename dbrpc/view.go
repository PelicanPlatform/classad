package dbrpc

import (
	"context"
	"encoding/json"

	"github.com/PelicanPlatform/classad/db"
)

// CreateView creates a materialized view named name from spec. The server materializes it
// synchronously and then maintains it live from the base table's change stream, so an error
// (e.g. the base table exceeds the view's cardinality limit) is reported here. A view is
// queried like a table (SELECT ... FROM <name>) but is read-only.
func (c *Client) CreateView(ctx context.Context, name string, spec db.ViewSpec) error {
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putBytes(putStr(req(id, opCreateView), name), data)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// DropView removes a materialized view (its definition and its in-memory data).
func (c *Client) DropView(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opDropView), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ListViews returns the materialized view names.
func (c *Client) ListViews(ctx context.Context) ([]string, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return req(id, opListViews) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		names = append(names, body.str())
	}
	return names, nil
}
