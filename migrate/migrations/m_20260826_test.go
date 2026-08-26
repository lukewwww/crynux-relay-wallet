package migrations

import (
	"crynux_relay_wallet/models"
	"math/big"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestM20260826DeprecatesOnlyExistingUndeletedActiveVesting(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&models.VestingRecord{},
		&models.RelayAccount{},
		&models.TaskFeeCheckpoint{},
	); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	activeNode := newMigrationVestingRecord(1, models.VestingStatusActive, models.VestingTypeNode, 30)
	activeOther := newMigrationVestingRecord(2, models.VestingStatusActive, models.VestingTypeOther, 365)
	completed := newMigrationVestingRecord(3, models.VestingStatusCompleted, models.VestingTypeDelegation, 90)
	deletedActive := newMigrationVestingRecord(4, models.VestingStatusActive, models.VestingTypeNode, 180)
	for _, record := range []*models.VestingRecord{activeNode, activeOther, completed, deletedActive} {
		if err := db.Create(record).Error; err != nil {
			t.Fatalf("create vesting %d: %v", record.RelayVestingID, err)
		}
	}
	if err := db.Delete(deletedActive).Error; err != nil {
		t.Fatalf("soft delete vesting: %v", err)
	}

	account := models.RelayAccount{
		Address: "0x5555555555555555555555555555555555555555",
		Balance: models.BigInt{Int: *big.NewInt(700)},
	}
	checkpoint := models.TaskFeeCheckpoint{
		LatestTaskFeeLogID:        88,
		LatestTaskFeeLogTimestamp: 99,
	}
	if err := db.Create(&account).Error; err != nil {
		t.Fatalf("create relay account: %v", err)
	}
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}

	migration := M20260826(db)
	if err := migration.Migrate(); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	assertMigrationVestingStatus(t, db, activeNode.ID, models.VestingStatusDeprecated)
	assertMigrationVestingStatus(t, db, activeOther.ID, models.VestingStatusDeprecated)
	assertMigrationVestingStatus(t, db, completed.ID, models.VestingStatusCompleted)
	assertMigrationVestingStatus(t, db, deletedActive.ID, models.VestingStatusActive)

	var migratedActive models.VestingRecord
	if err := db.Unscoped().First(&migratedActive, activeNode.ID).Error; err != nil {
		t.Fatalf("reload migrated active vesting: %v", err)
	}
	if migratedActive.TotalAmount.String() != "1000" ||
		migratedActive.ReleasedAmount.String() != "125" ||
		migratedActive.StartTime.Unix() != activeNode.StartTime.Unix() ||
		migratedActive.DurationDays != activeNode.DurationDays ||
		migratedActive.Type != activeNode.Type ||
		migratedActive.AdminSignature != activeNode.AdminSignature {
		t.Fatalf("migration changed vesting schedule fields: %+v", migratedActive)
	}

	newActive := newMigrationVestingRecord(5, models.VestingStatusActive, models.VestingTypeOther, 60)
	if err := db.Create(newActive).Error; err != nil {
		t.Fatalf("create post-migration vesting: %v", err)
	}
	assertMigrationVestingStatus(t, db, newActive.ID, models.VestingStatusActive)

	if err := migration.RollbackLast(); err != nil {
		t.Fatalf("rollback migration: %v", err)
	}
	assertMigrationVestingStatus(t, db, activeNode.ID, models.VestingStatusDeprecated)
	assertMigrationVestingStatus(t, db, activeOther.ID, models.VestingStatusDeprecated)
	assertMigrationVestingStatus(t, db, completed.ID, models.VestingStatusCompleted)
	assertMigrationVestingStatus(t, db, deletedActive.ID, models.VestingStatusActive)
	assertMigrationVestingStatus(t, db, newActive.ID, models.VestingStatusActive)

	var persistedAccount models.RelayAccount
	if err := db.First(&persistedAccount, account.ID).Error; err != nil {
		t.Fatalf("reload relay account: %v", err)
	}
	if persistedAccount.Balance.String() != "700" {
		t.Fatalf("relay account balance changed: %s", persistedAccount.Balance.String())
	}
	var persistedCheckpoint models.TaskFeeCheckpoint
	if err := db.First(&persistedCheckpoint, checkpoint.ID).Error; err != nil {
		t.Fatalf("reload checkpoint: %v", err)
	}
	if persistedCheckpoint.LatestTaskFeeLogID != 88 || persistedCheckpoint.LatestTaskFeeLogTimestamp != 99 {
		t.Fatalf("checkpoint changed: %+v", persistedCheckpoint)
	}
}

func newMigrationVestingRecord(relayID uint, status models.VestingStatus, vestingType string, durationDays uint) *models.VestingRecord {
	return &models.VestingRecord{
		RelayVestingID: relayID,
		Address:        "0x1111111111111111111111111111111111111111",
		TotalAmount:    models.BigInt{Int: *big.NewInt(1000)},
		ReleasedAmount: models.BigInt{Int: *big.NewInt(125)},
		StartTime:      time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		DurationDays:   durationDays,
		Type:           vestingType,
		AdminSignature: "signature",
		Status:         status,
	}
}

func assertMigrationVestingStatus(t *testing.T, db *gorm.DB, id uint, expected models.VestingStatus) {
	t.Helper()
	var record models.VestingRecord
	if err := db.Unscoped().First(&record, id).Error; err != nil {
		t.Fatalf("reload vesting %d: %v", id, err)
	}
	if record.Status != expected {
		t.Fatalf("vesting %d status = %d, want %d", id, record.Status, expected)
	}
}
