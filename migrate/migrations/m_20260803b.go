package migrations

import (
	"database/sql"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func M20260803b(db *gorm.DB) *gormigrate.Gormigrate {
	type BlockchainTransaction struct {
		SignedRawTx sql.NullString `gorm:"type:mediumtext;null"`
	}

	return gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		{
			ID: "M20260803b",
			Migrate: func(tx *gorm.DB) error {
				return tx.Migrator().AddColumn(&BlockchainTransaction{}, "SignedRawTx")
			},
			Rollback: func(tx *gorm.DB) error {
				return tx.Migrator().DropColumn(&BlockchainTransaction{}, "SignedRawTx")
			},
		},
	})
}
