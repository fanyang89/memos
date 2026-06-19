package profile

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValidate_DefaultMemoIndexInterval(t *testing.T) {
	t.Parallel()

	p := &Profile{}
	require.NoError(t, p.Validate())
	require.Equal(t, 5*time.Minute, p.MemoIndexInterval)
}

func TestValidate_NegativeMemoIndexIntervalCoercedToDefault(t *testing.T) {
	t.Parallel()

	p := &Profile{MemoIndexInterval: -1 * time.Second}
	require.NoError(t, p.Validate())
	require.Equal(t, 5*time.Minute, p.MemoIndexInterval)
}

func TestValidate_PositiveMemoIndexIntervalPreserved(t *testing.T) {
	t.Parallel()

	p := &Profile{MemoIndexInterval: 30 * time.Second}
	require.NoError(t, p.Validate())
	require.Equal(t, 30*time.Second, p.MemoIndexInterval)
}
