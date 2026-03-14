package httpbin

import (
	"testing"

	"github.com/galti3r/go-httpbin/v2/internal/testing/assert"
)

func TestParseMixDirectives(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		directives, err := parseMixDirectives("/mix/")
		assert.NilError(t, err)
		assert.Equal(t, len(directives), 0, "expected no directives")
	})

	t.Run("status_only", func(t *testing.T) {
		t.Parallel()
		directives, err := parseMixDirectives("/mix/s=503")
		assert.NilError(t, err)
		assert.Equal(t, len(directives), 1, "expected 1 directive")
		assert.Equal(t, directives[0].kind, "s", "wrong kind")
		assert.Equal(t, directives[0].value, "503", "wrong value")
	})

	t.Run("combined", func(t *testing.T) {
		t.Parallel()
		directives, err := parseMixDirectives("/mix/s=503/h=Retry-After:30/d=2s")
		assert.NilError(t, err)
		assert.Equal(t, len(directives), 3, "expected 3 directives")
	})

	t.Run("unknown_directive", func(t *testing.T) {
		t.Parallel()
		_, err := parseMixDirectives("/mix/x=foo")
		if err == nil {
			t.Fatal("expected error for unknown directive")
		}
	})

	t.Run("too_many", func(t *testing.T) {
		t.Parallel()
		path := "/mix/"
		for i := 0; i < 21; i++ {
			path += "s=200/"
		}
		_, err := parseMixDirectives(path)
		if err == nil {
			t.Fatal("expected error for too many directives")
		}
	})
}
