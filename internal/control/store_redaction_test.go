package control

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactDatabaseErrorRemovesURLAndPassword(t *testing.T) {
	databaseURL := "postgresql://panel:p%40ssword@db.example.test/panel"
	err := redactDatabaseError(errors.New("connect "+databaseURL+" failed with p@ssword"), databaseURL)
	if strings.Contains(err.Error(), databaseURL) || strings.Contains(err.Error(), "p@ssword") || strings.Contains(err.Error(), "p%40ssword") {
		t.Fatalf("database secret was not redacted: %v", err)
	}
}
