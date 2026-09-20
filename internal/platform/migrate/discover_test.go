package migrate

import (
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The FS shape mirrors the repository: modules sit one or two levels deep and
// share a single global numbering, so the number and the path disagree about
// order on purpose.
func repoLike() fstest.MapFS {
	return fstest.MapFS{
		"user/migrations/000001_init_users.up.sql":       {Data: []byte("CREATE TABLE users ();")},
		"user/migrations/000001_init_users.down.sql":     {Data: []byte("DROP TABLE users;")},
		"cms/content/migrations/000020_content.up.sql":   {Data: []byte("CREATE TABLE content ();")},
		"cms/content/migrations/000020_content.down.sql": {Data: []byte("DROP TABLE content;")},
		"auth/migrations/000003_auth.up.sql":             {Data: []byte("CREATE TABLE creds ();")},
	}
}

func TestDiscoverOrdersByNumberNotPath(t *testing.T) {
	// Sorting paths would put auth < cms < user and run 000003 before 000020
	// before 000001 — the numbering exists precisely because directory order is
	// not execution order.
	ms, err := Discover(repoLike())
	require.NoError(t, err)
	require.Len(t, ms, 3)
	assert.Equal(t, []int{1, 3, 20}, []int{ms[0].Version, ms[1].Version, ms[2].Version})
	assert.Equal(t, "init_users", ms[0].Name)
	assert.Equal(t, "cms/content/migrations/000020_content.up.sql", ms[2].Path)
}

func TestDiscoverIgnoresDownFiles(t *testing.T) {
	ms, err := Discover(repoLike())
	require.NoError(t, err)
	for _, m := range ms {
		assert.NotContains(t, m.Path, ".down.sql")
	}
}

func TestDiscoverRejectsDuplicateVersions(t *testing.T) {
	// The ledger keys on version, so a duplicate does not merely run out of
	// order — the second one looks permanently already-applied.
	fsys := repoLike()
	fsys["ticket/migrations/000003_tickets.up.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE t ();")}
	_, err := Discover(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "000003")
	assert.Contains(t, err.Error(), "claimed by both")
}

func TestDiscoverFailsOnEmptyTree(t *testing.T) {
	// An empty result must not read as "nothing to do": that is what a moved
	// layout looks like, and it would report a fresh database as fully migrated.
	_, err := Discover(fstest.MapFS{"README.md": {Data: []byte("hi")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no migrations found")
}

func TestChecksumTracksContent(t *testing.T) {
	before, err := Discover(repoLike())
	require.NoError(t, err)

	edited := repoLike()
	edited["auth/migrations/000003_auth.up.sql"] = &fstest.MapFile{
		Data: []byte("CREATE TABLE creds (id int);"),
	}
	after, err := Discover(edited)
	require.NoError(t, err)

	assert.Equal(t, before[0].Checksum, after[0].Checksum, "untouched migration changed checksum")
	assert.NotEqual(t, before[1].Checksum, after[1].Checksum,
		"editing an applied migration must be detectable — that is the whole point of storing a checksum")
}
