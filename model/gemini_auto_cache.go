package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GeminiAutoCacheSlot fences automatic creation across gateway instances. Key
// hashes the complete owner/provider binding and content prefix; no prompt or
// credential is stored. HoldUntil must cover the upstream resource's possible
// lifetime, including uncertain creation, before another request may claim it.
type GeminiAutoCacheSlot struct {
	Key        string `gorm:"type:varchar(64);primaryKey"`
	ClaimToken string `gorm:"type:varchar(64);not null"`
	PublicID   string `gorm:"type:varchar(64);not null;index:idx_gemini_auto_cache_cleanup,priority:1"`
	HoldUntil  int64  `gorm:"not null;index:idx_gemini_auto_cache_cleanup,priority:2"`
}

var ErrGeminiAutoCacheSlotInvalid = errors.New("invalid Gemini automatic cache slot")

func validGeminiAutoCacheDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func FindGeminiAutoCacheSlots(keys []string) ([]GeminiAutoCacheSlot, error) {
	if len(keys) > 256 {
		return nil, ErrGeminiAutoCacheSlotInvalid
	}
	slots := make([]GeminiAutoCacheSlot, 0)
	if len(keys) == 0 {
		return slots, nil
	}
	for _, key := range keys {
		if !validGeminiAutoCacheDigest(key) {
			return nil, ErrGeminiAutoCacheSlotInvalid
		}
	}
	err := DB.Where(map[string]any{"key": keys}).Find(&slots).Error
	return slots, err
}

func ClaimGeminiAutoCacheSlot(key string, now, holdUntil int64) (*GeminiAutoCacheSlot, bool, error) {
	if !validGeminiAutoCacheDigest(key) || now <= 0 || holdUntil <= now {
		return nil, false, ErrGeminiAutoCacheSlotInvalid
	}
	// Amortize removal of old failed/short-prefix probes. Published resources
	// stay indexed: explicit PATCH may have extended their original deadline.
	var expired []GeminiAutoCacheSlot
	if err := DB.Where("public_id = ? AND hold_until < ?", "", now-int64((24*time.Hour)/time.Second)).Limit(16).Find(&expired).Error; err != nil {
		return nil, false, err
	}
	for _, slot := range expired {
		// A concurrent replacement/publisher must never be deleted by cleanup.
		if err := DB.Where(map[string]any{"key": slot.Key, "claim_token": slot.ClaimToken, "hold_until": slot.HoldUntil, "public_id": ""}).Delete(&GeminiAutoCacheSlot{}).Error; err != nil {
			return nil, false, err
		}
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, false, err
	}
	candidate := &GeminiAutoCacheSlot{Key: key, ClaimToken: hex.EncodeToString(token[:]), HoldUntil: holdUntil}
	if err := DB.Clauses(clause.OnConflict{DoNothing: true}).Create(candidate).Error; err != nil {
		return nil, false, err
	}
	var current GeminiAutoCacheSlot
	if err := DB.Where(map[string]any{"key": key}).First(&current).Error; err != nil {
		return nil, false, err
	}
	// MySQL may report a duplicate insert as an affected row. Only the random
	// claim token proves that this request inserted the slot.
	if current.ClaimToken == candidate.ClaimToken {
		return candidate, true, nil
	}
	if current.HoldUntil > now {
		return &current, false, nil
	}
	result := DB.Model(&GeminiAutoCacheSlot{}).
		Where(map[string]any{"key": key, "claim_token": current.ClaimToken, "hold_until": current.HoldUntil}).
		Updates(map[string]any{"claim_token": candidate.ClaimToken, "public_id": "", "hold_until": holdUntil})
	if result.Error != nil {
		return nil, false, result.Error
	}
	if result.RowsAffected == 1 {
		return candidate, true, nil
	}
	if err := DB.Where(map[string]any{"key": key}).First(&current).Error; err != nil {
		return nil, false, err
	}
	return &current, false, nil
}

// FinishGeminiAutoCacheSlot publishes a settled cache, or records a cooldown
// with an empty publicID. An uncertain creation keeps its original hold. The
// claim token prevents a delayed creator from overwriting a replacement slot.
func FinishGeminiAutoCacheSlot(slot *GeminiAutoCacheSlot, publicID string, holdUntil int64) (bool, error) {
	if slot == nil || !validGeminiAutoCacheDigest(slot.Key) || !validGeminiAutoCacheDigest(slot.ClaimToken) ||
		len(publicID) > 64 || holdUntil <= 0 {
		return false, ErrGeminiAutoCacheSlotInvalid
	}
	result := DB.Model(&GeminiAutoCacheSlot{}).
		Where(map[string]any{"key": slot.Key, "claim_token": slot.ClaimToken}).
		Updates(map[string]any{"public_id": publicID, "hold_until": holdUntil})
	if result.Error != nil || result.RowsAffected > 0 {
		return result.RowsAffected == 1, result.Error
	}
	// A repeated finish can be a no-op on MySQL. Report the same successful
	// result as SQLite/PostgreSQL only while the exact claim and result remain.
	var current GeminiAutoCacheSlot
	err := DB.Where(map[string]any{"key": slot.Key, "claim_token": slot.ClaimToken, "public_id": publicID, "hold_until": holdUntil}).First(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}
