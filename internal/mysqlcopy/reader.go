package mysqlcopy

import (
	"bufio"
	"bytes"
	"io"
)

// validatedSQLReader forwards only whole validated statements. In particular,
// no unvalidated newline/client command is forwarded while waiting for a SQL
// semicolon. RAM is bounded to one statement, and the tracker keeps raw audit.
type validatedSQLReader struct {
	input   *bufio.Reader
	tracker *tracker
	ready   *bytes.Reader
	done    bool
}

func validateReader(r io.Reader, t *tracker) *validatedSQLReader {
	return &validatedSQLReader{input: bufio.NewReaderSize(r, 64<<10), tracker: t}
}
func (v *validatedSQLReader) exhausted() bool {
	return v.done && (v.ready == nil || v.ready.Len() == 0)
}
func (v *validatedSQLReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if v.ready != nil && v.ready.Len() > 0 {
		return v.ready.Read(p)
	}
	if v.done {
		return 0, io.EOF
	}
	var statement bytes.Buffer
	var quote byte
	escape := false
	for {
		c, err := v.input.ReadByte()
		if err != nil {
			if err != io.EOF {
				return 0, err
			}
			v.done = true
			if statement.Len() > 0 {
				if _, err = v.tracker.Write(statement.Bytes()); err != nil {
					return 0, err
				}
			}
			if _, err = v.tracker.Finish(); err != nil {
				return 0, err
			}
			if statement.Len() == 0 {
				return 0, io.EOF
			}
			v.ready = bytes.NewReader(statement.Bytes())
			return v.ready.Read(p)
		}
		if statement.Len() >= v.tracker.plan.MaxStatementBytes || int64(statement.Len()) >= v.tracker.plan.MaxBytes-v.tracker.bytes {
			return 0, ErrUnsafe
		}
		if err := statement.WriteByte(c); err != nil {
			return 0, err
		}
		if escape {
			escape = false
			continue
		}
		if quote != 0 {
			switch c {
			case '\\':
				escape = true
			case quote:
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			quote = c
			continue
		}
		if c == ';' {
			if _, err := v.tracker.Write(statement.Bytes()); err != nil {
				return 0, err
			}
			v.ready = bytes.NewReader(statement.Bytes())
			return v.ready.Read(p)
		}
	}
}
