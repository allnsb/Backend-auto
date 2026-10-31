package postgres

import (
	"strings"
	"testing"
)

func TestRejectsUnverifiedConnections(t *testing.T) {
	t.Setenv("PGSSLMODE", "")
	modes := []string{"disable", "allow", "prefer", "require", "verify-ca"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			_, err := parseConfig("postgresql://user:secret@localhost/test?sslmode=" + mode)
			if err == nil {
				t.Fatal("unverified database connection was accepted")
			}
		})
	}
}

func TestMalformedConnectionErrorDoesNotExposeSecret(t *testing.T) {
	const secret = "private-password-do-not-log"
	_, err := parseConfig("postgresql://user:" + secret + "@localhost:invalid/test")
	if err == nil {
		t.Fatal("malformed connection string was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("configuration error exposed the password")
	}
}
