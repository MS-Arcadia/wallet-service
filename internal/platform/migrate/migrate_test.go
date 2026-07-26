package migrate_test

import (
	"testing"
	"testing/fstest"

	"github.com/MS-Arcadia/wallet-service/internal/platform/migrate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSortsByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0010_add_holds.sql":      {Data: []byte("CREATE TABLE holds();")},
		"migrations/0002_create_ledger.sql":  {Data: []byte("CREATE TABLE ledger();")},
		"migrations/0001_create_wallets.sql": {Data: []byte("CREATE TABLE wallets();")},
		"migrations/README.md":               {Data: []byte("not a migration")},
	}

	migrations, err := migrate.Load(fsys, "migrations")
	require.NoError(t, err)
	require.Len(t, migrations, 3)

	assert.EqualValues(t, 1, migrations[0].Version)
	assert.Equal(t, "create_wallets", migrations[0].Name)
	assert.EqualValues(t, 2, migrations[1].Version)
	assert.EqualValues(t, 10, migrations[2].Version)
	assert.Equal(t, "add_holds", migrations[2].Name)
}

func TestLoadComputesStableChecksums(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE wallets();")},
	}
	first, err := migrate.Load(fsys, "migrations")
	require.NoError(t, err)
	second, err := migrate.Load(fsys, "migrations")
	require.NoError(t, err)

	assert.Equal(t, first[0].Checksum, second[0].Checksum)
	assert.Len(t, first[0].Checksum, 64, "sha256 hex digest")
}

func TestLoadChecksumChangesWithContent(t *testing.T) {
	original, err := migrate.Load(fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE wallets();")},
	}, "migrations")
	require.NoError(t, err)

	edited, err := migrate.Load(fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE wallets(id uuid);")},
	}, "migrations")
	require.NoError(t, err)

	assert.NotEqual(t, original[0].Checksum, edited[0].Checksum,
		"an edited migration must produce a different checksum so Up can refuse it")
}

func TestLoadRejectsBadFilenames(t *testing.T) {
	bad := []string{
		"migrations/init.sql",
		"migrations/_init.sql",
		"migrations/0001.sql",
		"migrations/abc_init.sql",
		"migrations/0000_init.sql",
	}
	for _, name := range bad {
		_, err := migrate.Load(fstest.MapFS{name: {Data: []byte("SELECT 1;")}}, "migrations")
		assert.ErrorIs(t, err, migrate.ErrBadFilename, "filename %q must be rejected", name)
	}
}

func TestLoadRejectsDuplicateVersions(t *testing.T) {
	_, err := migrate.Load(fstest.MapFS{
		"migrations/0001_a.sql": {Data: []byte("SELECT 1;")},
		"migrations/0001_b.sql": {Data: []byte("SELECT 2;")},
	}, "migrations")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "version 1")
}

func TestLoadRejectsEmptyDirectory(t *testing.T) {
	_, err := migrate.Load(fstest.MapFS{
		"migrations/README.md": {Data: []byte("nothing here")},
	}, "migrations")
	assert.ErrorIs(t, err, migrate.ErrNoMigrations)
}

func TestLoadMissingDirectory(t *testing.T) {
	_, err := migrate.Load(fstest.MapFS{}, "migrations")
	assert.Error(t, err)
}
