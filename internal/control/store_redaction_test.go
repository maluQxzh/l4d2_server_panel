package control

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactDatabaseErrorRemovesPath(t *testing.T) {
	databasePath := `C:\ProgramData\L4D2Panel\data\l4d2panel.sqlite3`
	err := redactDatabaseError(errors.New("open "+databasePath+" failed"), databasePath)
	if strings.Contains(err.Error(), databasePath) {
		t.Fatalf("database path was not redacted: %v", err)
	}
}
