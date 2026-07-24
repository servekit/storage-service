package fake

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/storage-service/internal/provider/storage/types"
)

// TestFakeProvider_SetObjectACL_HeadReturnsIt verifies that the ACL pinned via
// SetObjectACL is reported by HeadObject, enabling service-layer tests that
// assert ACL-verification behavior without spinning up an integration env.
func TestFakeProvider_SetObjectACL_HeadReturnsIt(t *testing.T) {
	ctx := context.Background()
	p := NewFakeProvider()

	require.NoError(t, p.PutObject(ctx, "b", "k", bytes.NewReader([]byte("x")), 1))

	p.SetObjectACL("b", "k", types.ObjectACLPublicRead)

	info, err := p.HeadObject(ctx, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, types.ObjectACLPublicRead, info.ObjectACL)
}

// TestFakeProvider_SetObjectACL_WithoutPut verifies SetObjectACL tolerates a
// missing prior PutObject — useful when a test wants to assert HeadObject-only
// behavior without setting up object bytes.
func TestFakeProvider_SetObjectACL_WithoutPut(t *testing.T) {
	ctx := context.Background()
	p := NewFakeProvider()

	p.SetObjectACL("b", "k", types.ObjectACLPrivate)

	info, err := p.HeadObject(ctx, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, types.ObjectACLPrivate, info.ObjectACL)
}

// TestFakeProvider_DefaultObjectACLEmpty verifies the zero-value path: objects
// PUT via PutObject alone must report empty ObjectACL, matching the
// "ACL unknown" semantics documented on types.ObjectInfo.
func TestFakeProvider_DefaultObjectACLEmpty(t *testing.T) {
	ctx := context.Background()
	p := NewFakeProvider()

	require.NoError(t, p.PutObject(ctx, "b", "k", bytes.NewReader([]byte("x")), 1))

	info, err := p.HeadObject(ctx, "b", "k")
	require.NoError(t, err)
	assert.Empty(t, info.ObjectACL, "object PUT without SetObjectACL must report empty ACL")
}
