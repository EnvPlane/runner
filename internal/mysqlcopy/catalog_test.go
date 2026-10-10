package mysqlcopy

import "testing"

func TestCatalogVisibilityAuthority(t *testing.T) {
	for _, grants := range []string{"GRANT SELECT ON `app`.* TO `reader`@`%`", "GRANT BACKUP_ADMIN ON *.* TO `backup`@`%`", "GRANT ALL PRIVILEGES ON `app`.* TO `backup`@`%`", "GRANT `backup_role`@`%` TO `reader`@`%`", "GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON `other`.* TO `backup`@`%`\nGRANT SHOW_ROUTINE ON *.* TO `backup`@`%`"} {
		if catalogAuthority("app", grants) {
			t.Fatal("filtered or wrong catalog accepted")
		}
	}
	if !catalogAuthority("app", "GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON `app`.* TO `backup`@`%`\nGRANT SHOW_ROUTINE, BACKUP_ADMIN ON *.* TO `backup`@`%`") {
		t.Fatal("explicit visibility denied")
	}
}
