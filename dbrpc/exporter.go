package dbrpc

import (
	"context"
	"encoding/json"

	"github.com/PelicanPlatform/classad/db"
)

// ExporterInfo is the name+kind pair returned by ListExporters. It deliberately omits the
// exporter's Config, which may hold credentials and is only returned by GetExporter to a
// DAEMON-authorized client.
type ExporterInfo struct {
	Name string
	Kind string
}

// CreateExporter registers an external-sink exporter definition. DAEMON-only. The server
// stores the definition (and later its resume state) but never runs the exporter -- an
// out-of-process exporter reads these back and does the work.
func (c *Client) CreateExporter(ctx context.Context, def db.ExporterDef) error {
	data, err := json.Marshal(def)
	if err != nil {
		return err
	}
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putBytes(req(id, opCreateExporter), data)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// DropExporter removes an exporter's definition and resume state. DAEMON-only.
func (c *Client) DropExporter(ctx context.Context, name string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opDropExporter), name) })
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ListExporters returns the registered exporters' names and kinds (no config).
func (c *Client) ListExporters(ctx context.Context) ([]ExporterInfo, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return req(id, opListExporters) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	out := make([]ExporterInfo, 0, n)
	for i := 0; i < n; i++ {
		info := ExporterInfo{Name: body.str()}
		info.Kind = body.str()
		out = append(out, info)
	}
	return out, nil
}

// GetExporter returns a single exporter's full definition (including Config). DAEMON-only.
// The bool is false when no exporter of that name exists.
func (c *Client) GetExporter(ctx context.Context, name string) (db.ExporterDef, bool, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opGetExporter), name) })
	if err != nil {
		return db.ExporterDef{}, false, err
	}
	if status != stOK {
		return db.ExporterDef{}, false, statusErr(status, body)
	}
	if body.u8() == 0 {
		return db.ExporterDef{}, false, nil
	}
	var def db.ExporterDef
	if err := json.Unmarshal(body.bytesRef(), &def); err != nil {
		return db.ExporterDef{}, false, err
	}
	return def, true, nil
}

// PutExporterState durably stores an exporter's opaque resume-state blob. DAEMON-only. The
// exporter calls this after downstream has accepted its data (the at-least-once boundary).
func (c *Client) PutExporterState(ctx context.Context, name string, state []byte) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putBytes(putStr(req(id, opPutExporterState), name), state)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// GetExporterState returns an exporter's last checkpointed resume-state blob. DAEMON-only.
// The bool is false when the exporter has never checkpointed (start from the beginning).
func (c *Client) GetExporterState(ctx context.Context, name string) ([]byte, bool, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return putStr(req(id, opGetExporterState), name) })
	if err != nil {
		return nil, false, err
	}
	if status != stOK {
		return nil, false, statusErr(status, body)
	}
	if body.u8() == 0 {
		return nil, false, nil
	}
	return body.bytesRef(), true, nil
}
