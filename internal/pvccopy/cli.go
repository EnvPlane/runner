package pvccopy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

type boundedBuffer struct {
	bytes.Buffer
	Limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.Len() {
		return 0, fmt.Errorf("bounded response exceeded")
	}
	return b.Buffer.Write(p)
}
func decodeJSON(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing JSON response")
	}
	return nil
}

// HelperCLI never initializes runner runtime or contacts Kubernetes. dir is
// fixed to /data by main; exposing it here permits real-byte local tests.
func HelperCLI(ctx context.Context, args []string, dir string, input io.Reader, output io.Writer) error {
	if len(args) < 2 || len(args) > 3 || len(args[1]) > 16384 {
		return fmt.Errorf("invalid helper arguments")
	}
	var p Plan
	if err := decodeJSON([]byte(args[1]), &p); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	var value any
	switch args[0] {
	case "pvc-export":
		if len(args) != 2 {
			return fmt.Errorf("invalid export arguments")
		}
		_, err := Export(ctx, dir, p, output)
		return err
	case "pvc-import":
		if len(args) != 2 {
			return fmt.Errorf("invalid import arguments")
		}
		r, err := Receive(ctx, dir, p, input)
		if err != nil {
			return err
		}
		value = r
	case "pvc-verify":
		if len(args) != 2 {
			return fmt.Errorf("invalid verify arguments")
		}
		m, err := Inspect(ctx, dir, p)
		if err != nil {
			return err
		}
		value = m
	case "pvc-complete":
		if len(args) != 3 || len(args[2]) > 1024 {
			return fmt.Errorf("missing completion receipt")
		}
		var r Receipt
		if err := decodeJSON([]byte(args[2]), &r); err != nil {
			return err
		}
		m, err := Complete(ctx, dir, p, r)
		if err != nil {
			return err
		}
		value = m
	default:
		return fmt.Errorf("unknown PVC helper command")
	}
	return json.NewEncoder(output).Encode(value)
}
