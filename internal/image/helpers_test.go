package image

import (
	"testing"

	"github.com/mghadia1/minibox/internal/testutil"
)

// Short names for the shared layer builders.
type tarEntry = testutil.Entry

var (
	file     = testutil.File
	folder   = testutil.Folder
	symlink  = testutil.Symlink
	hardlink = testutil.Hardlink
)

func makeTar(t testing.TB, entries ...tarEntry) []byte { return testutil.Tar(t, entries...) }

// testClient talks plain HTTP to a local fake registry.
func testClient() *Client {
	c := NewClient()
	c.Scheme = "http"
	return c
}
