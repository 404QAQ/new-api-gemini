package model

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	mysqlconfig "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This is the Token schema from v1.0.0-rc.44. User and Channel schemas are
// unchanged since that release. Keeping the old token schema exercises an
// actual released database upgrade, including its pre-existing unique index.
type geminiCacheReleasedToken struct {
	Id                 int
	UserId             int    `gorm:"index"`
	Key                string `gorm:"type:varchar(128);uniqueIndex"`
	Status             int    `gorm:"default:1"`
	Name               string `gorm:"index"`
	CreatedTime        int64  `gorm:"bigint"`
	AccessedTime       int64  `gorm:"bigint"`
	ExpiredTime        int64  `gorm:"bigint;default:-1"`
	RemainQuota        int    `gorm:"default:0"`
	UnlimitedQuota     bool
	ModelLimitsEnabled bool
	ModelLimits        string  `gorm:"type:text"`
	AllowIps           *string `gorm:"default:''"`
	UsedQuota          int     `gorm:"default:0"`
	Group              string  `gorm:"default:''"`
	CrossGroupRetry    bool
	AutoGroups         string         `gorm:"type:text"`
	DeletedAt          gorm.DeletedAt `gorm:"index"`
}

func (geminiCacheReleasedToken) TableName() string { return "tokens" }

// Configured engines use a newly created, uniquely named database, so neither
// full startup migration nor cleanup touches the database supplied in the DSN.
// The test account must have CREATE DATABASE permissions.
func openGeminiCacheTestDatabase(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	var dsn string
	switch dialect {
	case "sqlite":
		previousPath := common.SQLitePath
		common.SQLitePath = filepath.Join(t.TempDir(), "gemini-cache.db")
		t.Cleanup(func() { common.SQLitePath = previousPath })
		dsn = "local"
	case "mysql":
		dsn = strings.TrimSpace(os.Getenv("TEST_MYSQL_DSN"))
	case "postgres":
		dsn = strings.TrimSpace(os.Getenv("TEST_POSTGRES_DSN"))
	}
	if dsn == "" {
		t.Skip("TEST_" + strings.ToUpper(dialect) + "_DSN is not configured")
	}
	t.Setenv("GEMINI_CACHE_MIGRATION_TEST_DSN", dsn)
	admin, dbType, err := chooseDB("GEMINI_CACHE_MIGRATION_TEST_DSN", false)
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, adminSQL.Close()) })
	db := admin
	if dialect != "sqlite" {
		name, err := common.GenerateRandomCharsKey(16)
		require.NoError(t, err)
		name = "gemini_cache_test_" + strings.ToLower(name)
		require.NoError(t, admin.Exec("CREATE DATABASE ?", clause.Table{Name: name}).Error)
		t.Cleanup(func() { require.NoError(t, admin.Exec("DROP DATABASE ?", clause.Table{Name: name}).Error) })
		if dialect == "mysql" {
			config, err := mysqlconfig.ParseDSN(dsn)
			require.NoError(t, err)
			config.DBName = name
			dsn = config.FormatDSN()
		} else {
			parsed, err := url.Parse(dsn)
			require.NoError(t, err)
			require.Contains(t, []string{"postgres", "postgresql"}, parsed.Scheme, "use a PostgreSQL URL DSN")
			parsed.Path = "/" + name
			dsn = parsed.String()
		}
		t.Setenv("GEMINI_CACHE_MIGRATION_TEST_DSN", dsn)
		db, _, err = chooseDB("GEMINI_CACHE_MIGRATION_TEST_DSN", false)
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	if dialect == "sqlite" {
		sqlDB.SetMaxOpenConns(1)
	}
	previousDB, previousLogDB := DB, LOG_DB
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	DB, LOG_DB = db, db
	common.SetDatabaseTypes(dbType, dbType)
	initCol()
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMainType, previousLogType)
		initCol()
	})
	var version string
	versionQuery := "SELECT version()"
	if dialect == "sqlite" {
		versionQuery = "SELECT sqlite_version()"
	}
	require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database: %s", version)
	return db
}

func TestGeminiCachedContentDatabase(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for _, existingRelease := range []bool{false, true} {
				t.Run(fmt.Sprintf("released_database_%t", existingRelease), func(t *testing.T) {
					db := openGeminiCacheTestDatabase(t, dialect)
					if existingRelease {
						require.NoError(t, db.AutoMigrate(&User{}, &Channel{}, &geminiCacheReleasedToken{}))
						require.NoError(t, db.Create(&User{Id: 101, Username: "cache-existing-user", AuthVersion: 1, Quota: 12345}).Error)
						require.NoError(t, db.Create(&Channel{Id: 201, Name: "cache-existing-channel", Key: "fixture-channel-key"}).Error)
						require.NoError(t, db.Create(&geminiCacheReleasedToken{Id: 301, UserId: 101, Key: "fixture-token-key", RemainQuota: 4567, Group: "default"}).Error)
					}
					require.NoError(t, migrateDB())
					exists, err := GeminiCachedContentPublicIDExists(strings.Repeat("b", 32))
					require.NoError(t, err)
					assert.False(t, exists, "only unknown local IDs may be considered external caches")
					now := common.GetTimestamp()
					record := &GeminiCachedContent{
						PublicID: strings.Repeat("a", 32), UserID: 101, TokenID: 301, ChannelID: 201,
						Group: "default", KeyFingerprint: strings.Repeat("f", 64), BaseURL: "https://generativelanguage.googleapis.com",
						Model: "gemini-2.5-flash", UpstreamModel: "models/gemini-2.5-flash", UpstreamName: "cachedContents/upstream-a",
						DisplayName: "中文缓存", TotalTokens: 4096, ExpiresAt: now + 3600,
					}
					require.NoError(t, CreateGeminiCachedContent(record))
					require.Positive(t, record.ID)
					_, err = GetGeminiCachedContent(101, 301, record.PublicID)
					require.ErrorIs(t, err, gorm.ErrRecordNotFound, "pending resources cannot be used")
					changed, err := ActivateGeminiCachedContent(record)
					require.NoError(t, err)
					require.True(t, changed)
					changed, err = ActivateGeminiCachedContent(record)
					require.NoError(t, err)
					assert.False(t, changed, "activation cannot be replayed")

					recorder := &migrationSQLRecorder{}
					DB = db.Session(&gorm.Session{Logger: recorder})
					require.NoError(t, migrateDB())
					assert.Empty(t, recorder.schemaMutations(), "repeat startup migration must preserve the schema")
					DB = db
					loaded, err := GetGeminiCachedContent(101, 301, record.PublicID)
					require.NoError(t, err)
					assert.Equal(t, *record, *loaded, "migration must preserve exact binding, fingerprint, and accounting metadata")
					duplicate := *record
					duplicate.ID, duplicate.State = 0, GeminiCachedContentPending
					assert.Error(t, CreateGeminiCachedContent(&duplicate), "public resource IDs must remain unique")
					if existingRelease {
						var user User
						var channel Channel
						var token Token
						require.NoError(t, db.First(&user, 101).Error)
						require.NoError(t, db.First(&channel, 201).Error)
						require.NoError(t, db.First(&token, 301).Error)
						assert.Equal(t, "cache-existing-user", user.Username)
						assert.Equal(t, 12345, user.Quota)
						assert.Equal(t, "fixture-channel-key", channel.Key)
						assert.Equal(t, "fixture-token-key", token.Key)
						assert.Equal(t, 4567, token.RemainQuota)
						assert.False(t, token.AllowUserQuotaQuery)
						assert.Error(t, db.Create(&Token{UserId: 101, Key: "fixture-token-key"}).Error, "released unique constraints must survive")
					}

					for _, owner := range [][2]int{{102, 301}, {101, 302}, {102, 302}} {
						_, err := GetGeminiCachedContent(owner[0], owner[1], record.PublicID)
						assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
						_, err = GetGeminiCachedContentForRecovery(owner[0], owner[1], record.PublicID)
						assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
					}
					var secondID int64
					for i, fixture := range []struct {
						user, token int
						group       string
						state       string
						expires     int64
					}{
						{101, 301, "default", GeminiCachedContentActive, now + 3600},
						{102, 301, "default", GeminiCachedContentActive, now + 3600},
						{101, 302, "default", GeminiCachedContentActive, now + 3600},
						{101, 301, "other", GeminiCachedContentActive, now + 3600},
						{101, 301, "default", GeminiCachedContentActive, now - 1},
						{101, 301, "default", GeminiCachedContentPending, now + 3600},
						{101, 301, "default", GeminiCachedContentMutating, now + 3600},
						{101, 301, "default", GeminiCachedContentDeleted, now + 3600},
					} {
						copy := *record
						copy.ID, copy.PublicID = 0, fmt.Sprintf("%032x", i+1)
						copy.UserID, copy.TokenID, copy.Group = fixture.user, fixture.token, fixture.group
						copy.State, copy.ExpiresAt = fixture.state, fixture.expires
						require.NoError(t, db.Create(&copy).Error)
						exists, err := GeminiCachedContentPublicIDExists(copy.PublicID)
						require.NoError(t, err)
						assert.True(t, exists, "local IDs remain reserved across owners, groups, states, and expiry")
						if i == 0 {
							secondID = copy.ID
						}
						if fixture.expires <= now {
							_, err := GetGeminiCachedContent(101, 301, copy.PublicID)
							assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
						}
					}
					page, err := ListGeminiCachedContents(101, 301, "default", 0, 1)
					require.NoError(t, err)
					require.Len(t, page, 1)
					assert.Equal(t, secondID, page[0].ID)
					page, err = ListGeminiCachedContents(101, 301, "default", secondID, 100)
					require.NoError(t, err)
					require.Len(t, page, 1)
					assert.Equal(t, record.ID, page[0].ID)

					start := make(chan struct{})
					results := make(chan string, 2)
					errors := make(chan error, 2)
					var workers sync.WaitGroup
					for _, mutation := range []string{"mutation-one", "mutation-two"} {
						workers.Go(func() {
							<-start
							won, err := BeginGeminiCachedContentMutation(record.ID, mutation, now+120)
							errors <- err
							if won {
								results <- mutation
							}
						})
					}
					close(start)
					workers.Wait()
					close(results)
					close(errors)
					for err := range errors {
						require.NoError(t, err)
					}
					require.Len(t, results, 1, "only one upstream mutation may proceed")
					winner := <-results
					_, err = GetGeminiCachedContent(101, 301, record.PublicID)
					assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
					require.NoError(t, db.Model(&GeminiCachedContent{}).Where("id = ?", record.ID).Update("mutation_expires_at", now-1).Error)
					changed, err = BeginGeminiCachedContentMutation(record.ID, "replacement", now+120)
					require.NoError(t, err)
					assert.False(t, changed, "expired uncertain mutations must remain fenced")
					recovered, err := GetGeminiCachedContentForRecovery(101, 301, record.PublicID)
					require.NoError(t, err)
					assert.Equal(t, GeminiCachedContentMutating, recovered.State)
					changed, err = CancelGeminiCachedContentMutation(record.ID, "wrong-token")
					require.NoError(t, err)
					assert.False(t, changed)
					changed, err = CancelGeminiCachedContentMutation(record.ID, winner)
					require.NoError(t, err)
					require.True(t, changed)
					changed, err = BeginGeminiCachedContentMutation(record.ID, "next-mutation", now+120)
					require.NoError(t, err)
					require.True(t, changed)
					changed, err = FinishGeminiCachedContentMutation(record.ID, winner, now+1800, now+3600)
					require.NoError(t, err)
					assert.False(t, changed, "a previous owner cannot finalize a new mutation")
					changed, err = FinishGeminiCachedContentMutation(record.ID, "next-mutation", now+1800, now+1800)
					require.NoError(t, err)
					assert.False(t, changed, "paid storage cannot move backwards when TTL is shortened")
					changed, err = FinishGeminiCachedContentMutation(record.ID, "next-mutation", now+1800, now+3600)
					require.NoError(t, err)
					require.True(t, changed)
					loaded, err = GetGeminiCachedContent(101, 301, record.PublicID)
					require.NoError(t, err)
					assert.Equal(t, now+1800, loaded.ExpiresAt)
					assert.Equal(t, now+3600, loaded.PaidUntil)
					assert.Empty(t, loaded.MutationToken)
					assert.Zero(t, loaded.MutationExpiresAt)
					changed, err = SoftDeleteGeminiCachedContent(record.ID, "")
					require.NoError(t, err)
					assert.False(t, changed, "an active resource needs a mutation lease before deletion")
					changed, err = BeginGeminiCachedContentMutation(record.ID, "delete-mutation", now+120)
					require.NoError(t, err)
					require.True(t, changed)
					changed, err = SoftDeleteGeminiCachedContent(record.ID, "wrong-token")
					require.NoError(t, err)
					assert.False(t, changed)
					changed, err = SoftDeleteGeminiCachedContent(record.ID, "delete-mutation")
					require.NoError(t, err)
					require.True(t, changed)
					_, err = GetGeminiCachedContentForRecovery(101, 301, record.PublicID)
					assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
				})
			}
		})
	}
}
