package app

import (
	"testing"

	"go.uber.org/fx"
)

func TestComposition(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:local@localhost:5432/jungle")
	if err := fx.ValidateApp(Module, fx.NopLogger); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidConfigurationFailsBeforeStartup(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if err := New(fx.NopLogger).Err(); err == nil {
		t.Fatal("expected missing database configuration to fail")
	}
}
