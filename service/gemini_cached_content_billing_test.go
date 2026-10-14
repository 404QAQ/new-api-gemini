package service

import (
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func configureGeminiCacheBillingTest(t *testing.T) *billing_setting.BillingSetting {
	t.Helper()
	settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
	previous := *settings
	previousQuotaPerUnit := common.QuotaPerUnit
	t.Cleanup(func() { *settings = previous; common.QuotaPerUnit = previousQuotaPerUnit })
	common.QuotaPerUnit = 500000
	settings.BillingMode = map[string]string{
		"cache-test-cache-create":  billing_setting.BillingModeTieredExpr,
		"cache-test-cache-storage": billing_setting.BillingModeTieredExpr,
	}
	settings.BillingExpr = map[string]string{
		"cache-test-cache-create":  `tier("cache_create", p * 0.3 + p * (param("cache_storage_hours") ?? 0))`,
		"cache-test-cache-storage": `tier("cache_storage", p * (param("cache_storage_hours") ?? 0))`,
	}
	return settings
}

func TestGeminiCacheBillingQuote(t *testing.T) {
	settings := configureGeminiCacheBillingTest(t)
	for _, expression := range settings.BillingExpr {
		require.NoError(t, billing_setting.SmokeTestExpr(expression), "cache pricing must remain saveable through existing administration")
	}
	for _, tc := range []struct {
		name      string
		tokens    int
		duration  time.Duration
		operation GeminiCacheBillingOperation
		ratio     float64
		wantQuota int
		wantError bool
	}{
		{"creation includes input and one hour", 1000, time.Hour, GeminiCacheCreate, 1, 650, false},
		{"extension contains only storage", 1000, time.Hour, GeminiCacheStorage, 1, 500, false},
		{"group multiplier applies once", 1000, time.Hour, GeminiCacheCreate, 2, 1300, false},
		{"partial seconds round up", 1_000_000, time.Second + time.Nanosecond, GeminiCacheStorage, 1, 278, false},
		{"input-only failed creation", 1000, 0, GeminiCacheCreate, 1, 150, false},
		{"explicit free group", 1000, time.Hour, GeminiCacheCreate, 0, 0, false},
		{"zero upstream tokens", 0, time.Hour, GeminiCacheCreate, 1, 0, true},
		{"oversized upstream tokens", GeminiCacheMaxTokens + 1, time.Hour, GeminiCacheCreate, 1, 0, true},
		{"negative duration", 1000, -1, GeminiCacheCreate, 1, 0, true},
		{"duration above ceiling", 1000, GeminiCacheMaxTTL + 1, GeminiCacheCreate, 1, 0, true},
		{"negative multiplier", 1000, time.Hour, GeminiCacheCreate, -1, 0, true},
		{"nonfinite multiplier", 1000, time.Hour, GeminiCacheCreate, math.Inf(1), 0, true},
		{"unknown operation", 1000, time.Hour, "delete", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quote, err := QuoteGeminiCacheBilling("cache-test", tc.tokens, tc.duration, tc.operation, tc.ratio)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantQuota, quote.Quota)
		})
	}
	_, err := QuoteGeminiCacheBilling("unconfigured", 1000, time.Hour, GeminiCacheCreate, 1)
	require.ErrorContains(t, err, "pricing is not configured")
	settings.BillingExpr["cache-test-cache-create"] = `tier("free", p * 0)`
	quote, err := QuoteGeminiCacheBilling("cache-test", 1000, time.Hour, GeminiCacheCreate, 1)
	require.NoError(t, err)
	assert.Zero(t, quote.Quota)
	settings.BillingExpr["cache-test-cache-create"] = `tier("invalid", p * -1)`
	_, err = QuoteGeminiCacheBilling("cache-test", 1, time.Hour, GeminiCacheCreate, 1)
	require.ErrorContains(t, err, "non-negative")
	settings.BillingExpr["cache-test-cache-create"] = `tier("huge", p * 1e100)`
	quote, err = QuoteGeminiCacheBilling("cache-test", 1000, time.Hour, GeminiCacheCreate, 1)
	require.Error(t, err)
	require.NotNil(t, quote.QuotaClamp)
	assert.Equal(t, common.QuotaClampOverflow, quote.QuotaClamp.Kind)
}

// The optional DSNs must point to disposable test databases, as with the
// existing fixed-price accounting matrix. This exercises real wallet/token
// reservations and consume logs for all three supported primary databases.
func TestGeminiCacheBillingDatabaseMatrix(t *testing.T) {
	configureGeminiCacheBillingTest(t)
	previousRedis, previousBatch, previousConsume := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
	previousMaster := common.IsMasterNode
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, true
	common.IsMasterNode = false
	t.Setenv("LOG_SQL_DSN", "")
	t.Cleanup(func() {
		common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = previousRedis, previousBatch, previousConsume
		common.IsMasterNode = previousMaster
	})
	for _, dialect := range []struct {
		name common.DatabaseType
		env  string
	}{
		{common.DatabaseTypeSQLite, ""},
		{common.DatabaseTypeMySQL, "TEST_FIXED_MYSQL_DSN"},
		{common.DatabaseTypePostgreSQL, "TEST_FIXED_POSTGRES_DSN"},
	} {
		t.Run(string(dialect.name), func(t *testing.T) {
			var driver gorm.Dialector = sqlite.Open(":memory:")
			if dialect.env != "" {
				dsn := os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip(dialect.env + " is not configured")
				}
				if dialect.name == common.DatabaseTypeMySQL {
					driver = mysql.Open(dsn)
				} else {
					driver = postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true})
				}
			}
			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
			previousDB, previousLogs := model.DB, model.LOG_DB
			previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, model.LOG_DB = db, db
			common.SetDatabaseTypes(dialect.name, dialect.name)
			require.NoError(t, model.InitLogDB(), "initialize dialect-specific reserved column names")
			t.Cleanup(func() {
				model.DB, model.LOG_DB = previousDB, previousLogs
				common.SetDatabaseTypes(previousMainType, previousLogType)
				require.NoError(t, model.InitLogDB())
				model.LOG_DB = previousLogs
				common.SetDatabaseTypes(previousMainType, previousLogType)
			})
			require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Log{}))
			versionQuery := "SELECT version()"
			if dialect.name == common.DatabaseTypeSQLite {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)
			for _, tc := range []struct {
				name                 string
				wallet, token        int
				actualTokens         int
				wantCharge           int
				reserveFails, refund bool
				priceChanges         bool
				initialFree          bool
				groupRatio           float64
			}{
				{name: "actual usage settles once", wallet: 10000, token: 10000, actualTokens: 2000, wantCharge: 1300, priceChanges: true},
				{name: "lower actual usage refunds difference", wallet: 10000, token: 10000, actualTokens: 500, wantCharge: 325},
				{name: "wallet insufficiency cannot create debt", wallet: 1000, token: 10000, actualTokens: 2000, reserveFails: true, refund: true},
				{name: "token insufficiency rolls back wallet supplement", wallet: 10000, token: 1000, actualTokens: 2000, reserveFails: true, refund: true},
				{name: "failure refunds original and supplemental reservation", wallet: 10000, token: 10000, actualTokens: 2000, refund: true},
				{name: "free group needs no wallet or token balance", wallet: 0, token: 0, actualTokens: 2000, groupRatio: -1},
				{name: "initial free tier reserves when actual usage is paid", wallet: 10000, token: 10000, actualTokens: 2000, wantCharge: 1300, initialFree: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					user := model.User{Username: "cache-" + common.GetUUID(), AffCode: common.GetUUID(), Quota: tc.wallet, Status: common.UserStatusEnabled}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: common.GetUUID(), Name: "cache-test", RemainQuota: tc.token, Status: common.TokenStatusEnabled}
					require.NoError(t, db.Create(&token).Error)
					channel := model.Channel{Type: 24, Name: "cache-test"}
					require.NoError(t, db.Create(&channel).Error)
					t.Cleanup(func() {
						require.NoError(t, db.Delete(&model.Log{}, "user_id = ?", user.Id).Error)
						require.NoError(t, db.Delete(&token).Error)
						require.NoError(t, db.Delete(&user).Error)
						require.NoError(t, db.Delete(&channel).Error)
					})
					ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
					ctx.Request = httptest.NewRequest("POST", "/v1beta/cachedContents", nil)
					ctx.Set("token_name", token.Name)
					info := &relaycommon.RelayInfo{
						UserId: user.Id, TokenId: token.Id, TokenKey: token.Key,
						OriginModelName: "cache-test", StartTime: time.Now(),
						ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, UpstreamModelName: "upstream-model"},
						UsingGroup:  "default", UserGroup: "default",
						UserSetting: dto.UserSetting{BillingPreference: "wallet_only"},
					}
					groupRatio, initialQuota := float64(1), 650
					if tc.groupRatio == -1 {
						groupRatio, initialQuota = 0, 0
					}
					if tc.initialFree {
						settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
						previous := settings.BillingExpr["cache-test-cache-create"]
						settings.BillingExpr["cache-test-cache-create"] = `len <= 1000 ? tier("free", p * 0) : tier("paid", p * 0.3 + p * (param("cache_storage_hours") ?? 0))`
						t.Cleanup(func() { settings.BillingExpr["cache-test-cache-create"] = previous })
						initialQuota = 0
					}
					quote, err := QuoteGeminiCacheBilling("cache-test", 1000, time.Hour, GeminiCacheCreate, groupRatio)
					require.NoError(t, err)
					session, apiErr := NewGeminiCacheBillingSession(ctx, info, quote)
					require.Nil(t, apiErr)
					assert.Equal(t, "cache-test", info.OriginModelName)
					assert.Equal(t, "upstream-model", info.UpstreamModelName)
					if initialQuota == 0 {
						assert.Nil(t, info.Billing)
					} else {
						assert.Equal(t, initialQuota, info.Billing.GetPreConsumedQuota())
					}
					if tc.priceChanges {
						settings := config.GlobalConfig.Get("billing_setting").(*billing_setting.BillingSetting)
						previous := settings.BillingExpr[quote.BillingModel]
						settings.BillingExpr[quote.BillingModel] = `tier("changed", p * 100)`
						t.Cleanup(func() { settings.BillingExpr[quote.BillingModel] = previous })
					}
					err = session.ReserveActual(tc.actualTokens, time.Hour)
					if tc.reserveFails {
						require.Error(t, err)
						assert.Equal(t, 650, info.Billing.GetPreConsumedQuota())
						balance, balanceErr := model.GetUserQuota(user.Id, true)
						require.NoError(t, balanceErr)
						assert.Equal(t, tc.wallet-650, balance)
					} else {
						require.NoError(t, err)
					}
					if tc.refund {
						refunded := make(chan struct{}, 1)
						const callback = "cache_billing_refund_observed"
						require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
							if tx.Statement.Table == "tokens" && tx.Error == nil {
								select {
								case refunded <- struct{}{}:
								default:
								}
							}
						}))
						t.Cleanup(func() { require.NoError(t, db.Callback().Update().Remove(callback)) })
						session.Refund(ctx)
						session.Refund(ctx)
						select {
						case <-refunded:
						case <-time.After(5 * time.Second):
							t.Fatal("cache billing refund did not finish")
						}
					} else {
						require.NoError(t, session.SettleAndLog(ctx, "public-cache"))
						require.NoError(t, session.SettleAndLog(ctx, "public-cache"))
						var logs []model.Log
						require.NoError(t, db.Where("user_id = ?", user.Id).Find(&logs).Error)
						require.Len(t, logs, 1)
						assert.Equal(t, tc.wantCharge, logs[0].Quota)
						assert.Equal(t, "cache-test-cache-create", logs[0].ModelName)
						var other map[string]any
						require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
						assert.Equal(t, float64(tc.actualTokens), other["cache_creation_input_tokens"])
						assert.Equal(t, float64(3600), other["cache_storage_seconds"])
						assert.Equal(t, "cachedContents/public-cache", other["cached_content"])
					}
					var actualUser model.User
					var actualToken model.Token
					require.NoError(t, db.First(&actualUser, user.Id).Error)
					require.NoError(t, db.First(&actualToken, token.Id).Error)
					assert.Equal(t, tc.wallet-tc.wantCharge, actualUser.Quota, fmt.Sprintf("wallet after %s", tc.name))
					assert.Equal(t, tc.token-tc.wantCharge, actualToken.RemainQuota)
				})
			}
		})
	}
}
