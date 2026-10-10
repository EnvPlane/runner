package mysqlcopy

import "strings"

// information_schema hides objects from insufficiently privileged accounts.
// Only explicit direct grants proving full catalog visibility are accepted;
// roles/definer ownership/zero counts alone cannot prove absence. The reviewed
// separate admin account needs SELECT, SHOW VIEW, TRIGGER, EVENT on this schema
// and global SHOW_ROUTINE (or global ALL PRIVILEGES). Our code performs only
// reads and the backup lock, never uses those catalog privileges for DDL.
func catalogAuthority(db, grants string) bool {
	permissions := map[string]bool{}
	routine := false
	for _, line := range strings.Split(grants, "\n") {
		if !strings.HasPrefix(line, "GRANT ") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(line, "GRANT "), " ON ", 2)
		if len(parts) != 2 {
			continue
		}
		scope := strings.SplitN(parts[1], " TO ", 2)
		if len(scope) != 2 {
			continue
		}
		global := scope[0] == "*.*"
		if !global && scope[0] != "`"+db+"`.*" {
			continue
		}
		for _, priv := range strings.Split(parts[0], ",") {
			priv = strings.TrimSpace(priv)
			if priv == "ALL PRIVILEGES" {
				for _, p := range []string{"SELECT", "SHOW VIEW", "TRIGGER", "EVENT"} {
					permissions[p] = true
				}
				if global {
					routine = true
				}
			} else {
				permissions[priv] = true
				if global && priv == "SHOW_ROUTINE" {
					routine = true
				}
			}
		}
	}
	return routine && permissions["SELECT"] && permissions["SHOW VIEW"] && permissions["TRIGGER"] && permissions["EVENT"]
}
