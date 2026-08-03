package migrations

import (
	"database/sql"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func M20260803(db *gorm.DB) *gormigrate.Gormigrate {
	type BlockchainTransaction struct {
		CancellationRequestedAt sql.NullTime `gorm:"null"`
	}

	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "M20260803",
			Migrate: func(tx *gorm.DB) error {
				return tx.Migrator().AddColumn(&BlockchainTransaction{}, "CancellationRequestedAt")
			},
			Rollback: func(tx *gorm.DB) error {
				return tx.Migrator().DropColumn(&BlockchainTransaction{}, "CancellationRequestedAt")
			},
		},
	})
}
