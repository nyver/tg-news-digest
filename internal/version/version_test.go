package version

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersion_DefaultValue(t *testing.T) {
	// By default, Version is set to "dev" at compile time
	assert.Equal(t, "dev", Version)
}

func TestVersion_IsString(t *testing.T) {
	// Version should be a non-empty string
	assert.IsType(t, "", Version)
	assert.NotEmpty(t, Version)
}
