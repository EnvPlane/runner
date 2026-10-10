package mysqlcopy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"regexp"
	"sort"
	"strings"
)

type tableTrack struct {
	schema string
	rows   int64
	hash   hash.Hash
}
type tracker struct {
	plan    Plan
	bytes   int64
	hash    hash.Hash
	pending bytes.Buffer
	quote   byte
	escape  bool
	tables  map[string]*tableTrack
	rows    int64
	failed  bool
}

func newTracker(p Plan) *tracker {
	return &tracker{plan: p, hash: sha256.New(), tables: map[string]*tableTrack{}}
}

var createTable = regexp.MustCompile("^CREATE TABLE `([A-Za-z][A-Za-z0-9_]{0,63})` ")
var insertTable = regexp.MustCompile("^INSERT INTO `([A-Za-z][A-Za-z0-9_]{0,63})` ")
var sessionSet = regexp.MustCompile(`^/\*![0-9]{5} SET (?:@saved_cs_client\s+=\s+@@character_set_client|character_set_client\s+=\s+(?:utf8mb4|@saved_cs_client)) \*/;$`)

func (t *tracker) Write(b []byte) (int, error) {
	if t.failed || int64(len(b)) > t.plan.MaxBytes-t.bytes {
		t.failed = true
		return 0, ErrUnsafe
	}
	for _, c := range b {
		if t.pending.Len() >= t.plan.MaxStatementBytes {
			t.failed = true
			return 0, ErrUnsafe
		}
		if err := t.pending.WriteByte(c); err != nil {
			return 0, err
		}
		if t.escape {
			t.escape = false
			continue
		}
		if t.quote != 0 {
			switch c {
			case '\\':
				t.escape = true
			case t.quote:
				t.quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' || c == '`' {
			t.quote = c
			continue
		}
		if c == ';' {
			if t.statement(t.pending.String()) != nil {
				t.failed = true
				return 0, ErrUnsafe
			}
			t.pending.Reset()
		}
	}
	t.bytes += int64(len(b))
	_, _ = t.hash.Write(b)
	return len(b), nil
}
func (t *tracker) statement(raw string) error {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "/*!") {
		// mysqldump's session-only SET wrappers. Never allow account/global/admin
		// SQL, stored programs, executable DDL comments, or arbitrary SELECT.
		if !sessionSet.MatchString(s) {
			return ErrUnsafe
		}
		return nil
	}
	if m := createTable.FindStringSubmatch(s); m != nil {
		if len(t.tables) >= t.plan.MaxTables || t.tables[m[1]] != nil {
			return ErrUnsafe
		}
		u := strings.ToUpper(s)
		for _, bad := range []string{"DATA DIRECTORY", "INDEX DIRECTORY", "TABLESPACE", "DEFINER", "SELECT ", "/*"} {
			if strings.Contains(u, bad) {
				return ErrUnsafe
			}
		}
		if !strings.Contains(u, "ENGINE=INNODB") && (t.plan.DDLMode != "quiescence" || (!strings.Contains(u, "ENGINE=MYISAM") && !strings.Contains(u, "ENGINE=MEMORY"))) {
			return ErrUnsafe
		}
		t.tables[m[1]] = &tableTrack{schema: hashString([]byte(normalizeSchema(s))), hash: sha256.New()}
		return nil
	}
	if m := insertTable.FindStringSubmatch(s); m != nil {
		v := t.tables[m[1]]
		if v == nil || t.rows >= t.plan.MaxRows {
			return ErrUnsafe
		}
		i := strings.Index(s, " VALUES (")
		if i < 0 || !singleValuesTuple(s[i+8:]) {
			return ErrUnsafe
		}
		v.rows++
		t.rows++
		_, _ = v.hash.Write([]byte(s))
		return nil
	}
	return ErrUnsafe
}
func singleValuesTuple(s string) bool {
	depth := 0
	var quote byte
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
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
		if c == '(' {
			depth++
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[i+1:]) == ";"
			}
			if depth < 0 {
				return false
			}
		}
	}
	return false
}
func (t *tracker) Finish() (Proof, error) {
	if t.failed || t.quote != 0 || strings.TrimSpace(t.pending.String()) != "" || len(t.tables) == 0 {
		return Proof{}, ErrUnsafe
	}
	names := make([]string, 0, len(t.tables))
	for n := range t.tables {
		names = append(names, n)
	}
	sort.Strings(names)
	r := Proof{Bytes: t.bytes, SHA256: hex.EncodeToString(t.hash.Sum(nil))}
	for _, n := range names {
		v := t.tables[n]
		r.Tables = append(r.Tables, TableProof{n, v.rows, v.schema, hex.EncodeToString(v.hash.Sum(nil))})
	}
	return r, nil
}
